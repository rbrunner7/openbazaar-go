package monero

import (
	"errors"
	"log"
	"math"
	"net/url"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/OpenBazaar/multiwallet/config"
	wallet "github.com/OpenBazaar/wallet-interface"
	wi "github.com/OpenBazaar/wallet-interface"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	btc "github.com/btcsuite/btcutil"
	hd "github.com/btcsuite/btcutil/hdkeychain"
	logging "github.com/op/go-logging"

	// "github.com/rbrunner7/go-monero/walletrpc"
	walletrpc "github.com/rbrunner7/go-monero-rpc-client/wallet"
	"golang.org/x/net/proxy"
)

// Name of the attribute stored in the Monero wallet to know where to report
// transactions from for listeners
const reportHeightAttribute string = "openbazaar.report_height"

// Default address for reaching the "monero-wallet-rpc" server;
// During beta use port 38083 which is usually associated with using the RPC
// server for stagenet, not mainnet
const defaultRpcAddress string = "http://127.0.0.1:38083/json_rpc"

const mainnetAddressPrefix string = "4"
const stagenetAddressPrefix string = "9"

// Convert from a human-readable XMR amount to XMR atomic units
// Note that while BTC has 8 decimal digits Monero has 12
func AmountToAtomicUnits(amount float64) int64 {
	return int64(amount * 1E12)
}

// Convert XMR atomic units to a human-readable XMR amount
func AtomicUnitsToAmount(units int64) float64 {
	return float64(units) / 1E12
}

type Wallet struct {
	rpcAddress   string
	client       walletrpc.Client
	testnet      bool
	logToFile    bool
	fileLog      *logging.Logger
	logToConsole bool
	consoleLog   *logging.Logger
	priceFetcher wi.ExchangeRates
	mutex        sync.Mutex
	listeners    []func(wi.TransactionCallback)
	reportHeight uint64
	mainAddress  string
	urlErrors    int
}

func (w *Wallet) logInfo(message string, args ...interface{}) {
	if w.logToFile {
		w.fileLog.Infof(message, args...)
	}
	if w.logToConsole {
		w.consoleLog.Infof(message, args...)
	}
}

// Log an error
// There is a tricky problem with error reporting here because the server always starts ALL wallets.
// If somebody does not use Monero and therefore has no RPC wallet running this could lead to
// an endless stream of "cannot connect" error messages, at least one every 15 seconds when the
// server checks for changes in the balance. Somewhat hacky workaround: After reporting a
// connection problem a given number of times just stop to report it further.
// (Of course one could simply lock the wallet and idle any calls to it if the 'Start' method
// cannot successfully connect to an RPC wallet instance, but that would not be very robust: You
// could not start the RPC wallet AFTER the server. Now it just connects belatedly.)
func (w *Wallet) logError(err interface{}) {
	var message string = ""
	if err != nil {
		baseError, ok := err.(error)
		if ok {
			message += baseError.Error()
		}
		_, ok = err.(*url.Error)
		if ok {
			// Almost for sure the problem is that we do not have a Monero RPC wallet
			// running or at least can't reach it with the URL given
			w.urlErrors++
			if w.urlErrors > 3 {
				// Already reported that too many times, don't report further - see
				// comment at method header above
				return
			}
		}
	}
	if w.logToFile {
		w.fileLog.Errorf(message)
	}
	if w.logToConsole {
		w.consoleLog.Errorf(message)
	}
}

func (w *Wallet) logDebug(message string, args ...interface{}) {
	if w.logToFile {
		w.fileLog.Debugf(message, args...)
	}
	if w.logToConsole {
		w.consoleLog.Debugf(message, args...)
	}
}

func multisigNotImplementedError(methodName string) error {
	err := errors.New("Method '" + methodName + "': Multisig is not implemented for Monero")
	return err
}

func (w *Wallet) lock() {
	// The Monero wallet RPC interface (and the wallet code behind it in general) is not thread-safe.
	// The go bindings used for accessing the interface do not care about this. This means we have
	// to synchronize ourselves here.
	// Be careful not to call a method that locks within another one which also locks because that
	// would of course deadlock.
	w.mutex.Lock()
}

func (w *Wallet) unlock() {
	w.mutex.Unlock()
}

// Get the main address of this wallet
// Depending on 'testnet' check whether we have the right type of address and return
// an error if not, e.g. to protect the user from inadvertently spending real XMR
// when in test mode. Note that the Monero RPC wallet gets started completely
// independently from this OpenBazaar wallet code, so it is free to connect to any net
// without we here able to influence anything there. But we recognize the net from
// the wallet's main address and can refuse a net we don't want to be connected to.
//
// Accept both Stagenet and Testnet as test nets in the sense of OpenBazaar.
// (Monero Stagenet runs on release code, whereas Testnet often runs on
// beta dev code not compatible with release code anymore.)
func (w *Wallet) getMainAddress() error {
	req := walletrpc.RequestGetAddress{AccountIndex: 0, AddressIndex: []uint64{0}}
	resp, err := w.client.GetAddress(&req)
	if err != nil {
		w.logError(err)
		// If we can't get the address most probably the RPC wallet is not yet running.
		// Do NOT return this connection error so retries can be made later, for making
		// the whole system more robust / tolerant.
		return nil
	}
	w.mainAddress = resp.Addresses[0].Address
	w.logInfo("Main address %s", w.mainAddress)
	validateReq := walletrpc.RequestValidateAddress{Address: w.mainAddress, AnyNetType: true, AllowOpenAlias: false}
	validateResp, err := w.client.ValidateAddress(&validateReq)
	if err != nil {
		w.logError(err)
		return err
	}
	if w.testnet {
		if validateResp.NetType == "mainnet" {
			err := errors.New("mainnet address used in test mode")
			w.logError(err)
			return err
		}
	} else {
		if validateResp.NetType != "mainnet" {
			err := errors.New(validateResp.NetType + " address used in mainnet mode")
			w.logError(err)
			return err
		}
	}
	return nil
}

func (w *Wallet) Start() {
	// Made contact to the RPC wallet already in 'NewMoneroWallet', for being able to reject
	// wallets on the wrong net, so almost nothing is left to do here
	w.logInfo("")
	go w.listenerThread()
}

func (w *Wallet) CurrencyCode() string {
	w.logDebug("")
	if w.testnet {
		return "txmr"
	} else {
		return "xmr"
	}
}

func (w *Wallet) IsDust(amount int64) bool {
	// Part of multisig processing, but can't give back "notImplementedError"
	err := multisigNotImplementedError("IsDust")
	w.logError(err)
	return false
}

func (w *Wallet) ChildKey(keyBytes []byte, chaincode []byte, isPrivateKey bool) (*hd.ExtendedKey, error) {
	err := multisigNotImplementedError("ChildKey")
	w.logError(err)
	return nil, err
}

func (w *Wallet) ScriptToAddress(script []byte) (btc.Address, error) {
	err := multisigNotImplementedError("ScriptToAddress")
	w.logError(err)
	return nil, err
}

func (w *Wallet) HasKey(addr btc.Address) bool {
	// Part of multisig processing, but can't give back "notImplementedError"
	err := multisigNotImplementedError("HasKey")
	w.logError(err)
	return false
}

func (w *Wallet) CurrentAddress(purpose wi.KeyPurpose) btc.Address {
	w.logInfo("Get address for purpose %d", purpose)
	w.lock()
	defer w.unlock()
	req := walletrpc.RequestGetAddress{AccountIndex: 0, AddressIndex: []uint64{}}
	resp, err := w.client.GetAddress(&req)
	if err != nil {
		w.logError(err)
		// Don't give back "nil" because this will lead to a panic right away
		return NewMoneroAddress("")
	}
	for _, addressInfo := range resp.Addresses {
		if !addressInfo.Used {
			w.logInfo("Existing subaddress %s", addressInfo.Address)
			return NewMoneroAddress(addressInfo.Address)
		}
	}
	return w.newAddressUnlocked(purpose)
}

func (w *Wallet) newAddressUnlocked(purpose wi.KeyPurpose) btc.Address {
	req := walletrpc.RequestCreateAddress{AccountIndex: 0}
	resp, err := w.client.CreateAddress(&req)
	if err != nil {
		w.logError(err)
		return nil
	}
	w.logInfo("New subaddress %s", resp.Address)
	return NewMoneroAddress(resp.Address)
}

// Create a new address
// Sadly we don't get to learn the order id already here because it would be quite easy
// to set here right away as the label of the new address, to document what it was used for.
// Later, in 'Spend', it's much more complicated to set the label retroactively, and thus
// this is not implemented (yet).
func (w *Wallet) NewAddress(purpose wi.KeyPurpose) btc.Address {
	w.logInfo("Create new address for purpose %d", purpose)
	w.lock()
	defer w.unlock()
	return w.newAddressUnlocked(purpose)
}

func (w *Wallet) DecodeAddress(addr string) (btc.Address, error) {
	w.logInfo("Decode address '%s'", addr)
	w.lock()
	defer w.unlock()
	req := walletrpc.RequestValidateAddress{Address: addr, AnyNetType: true, AllowOpenAlias: false}
	resp, err := w.client.ValidateAddress(&req)
	if err != nil {
		w.logError(errors.New(err.Error() + " '" + addr + "'"))
		return nil, err
	}
	w.logInfo("NetType %s, subaddress %t, valid %t", resp.NetType, resp.Subaddress, resp.Valid)
	if !resp.Valid {
		err := errors.New("Not valid as a Monero address '" + addr + "'")
		w.logError(err)
		return nil, err
	}
	return NewMoneroAddress(addr), nil
}

func (w *Wallet) Balance() (confirmed, unconfirmed int64) {
	// Only debug log because it gets called every 15 seconds
	w.logDebug("")
	w.lock()
	defer w.unlock()
	req := walletrpc.RequestGetBalance{}
	resp, err := w.client.GetBalance(&req)
	if err != nil {
		w.logError(err)
		return 0, 0
	} else {
		// Should we loose contact to the RPC wallet again later report those new URL errors
		w.urlErrors = 0
	}
	balance := resp.Balance
	unlockedBalance := resp.UnlockedBalance
	w.logDebug("Total %.12f, unlocked %.12f", AtomicUnitsToAmount(int64(balance)), AtomicUnitsToAmount(int64(unlockedBalance)))
	return int64(balance), int64(balance - unlockedBalance)
}

func transferToTxn(transfer *walletrpc.Transfer, sign int64, blockchainHeight uint64) wi.Txn {
	return wi.Txn{Txid: transfer.TxID, Value: int64(transfer.Amount) * sign, Height: int32(transfer.Height),
		Timestamp: time.Unix(int64(transfer.Timestamp), 0), WatchOnly: false,
		Confirmations: int64(blockchainHeight - transfer.Height), Status: wi.StatusConfirmed,
	}
}

func (w *Wallet) transactionsUnlocked() ([]wi.Txn, error) {
	heightResp, err := w.client.GetHeight()
	if err != nil {
		w.logError(err)
		return nil, err
	}
	req := walletrpc.RequestGetTransfers{In: true, Out: true}
	resp, err := w.client.GetTransfers(&req)
	if err != nil {
		w.logError(err)
		return nil, err
	}
	transactions := make([]wi.Txn, 0, 0)
	for _, transfer := range resp.In {
		transaction := transferToTxn(transfer, 1, heightResp.Height)
		transactions = append(transactions, transaction)
	}
	for _, transfer := range resp.Out {
		transaction := transferToTxn(transfer, -1, heightResp.Height)
		transactions = append(transactions, transaction)
	}
	// The client does not seem to sort itself, so do it for it here
	sort.Slice(transactions, func(i, j int) bool {
		return transactions[i].Height < transactions[j].Height
	})
	w.logInfo("%d in txs, %d out txs", len(resp.In), len(resp.Out))
	return transactions, nil
}

func (w *Wallet) Transactions() ([]wi.Txn, error) {
	w.logInfo("")
	w.lock()
	defer w.unlock()
	return w.transactionsUnlocked()
}

func (w *Wallet) GetTransaction(txid chainhash.Hash) (wi.Txn, error) {
	txidString := txid.String()
	w.logInfo("Get transaction %s", txidString)
	w.lock()
	defer w.unlock()
	heightResp, err := w.client.GetHeight()
	if err != nil {
		w.logError(err)
		return wi.Txn{}, err
	}
	req := walletrpc.RequestGetTransferByTxID{AccountIndex: 0, TxID: txidString}
	transferResp, err := w.client.GetTransferByTxID(&req)
	if err != nil {
		w.logError(err)
		return wi.Txn{}, err
	}
	var sign int64 = 1
	if transferResp.Transfer.Type == "out" {
		sign = -1
	}
	w.logInfo("Height %d", heightResp.Height)
	return transferToTxn(&transferResp.Transfer, sign, heightResp.Height), nil
}

func (w *Wallet) ChainTip() (uint32, chainhash.Hash) {
	// This info is not available through the Monero wallet RPC, and connecting openbazaard to
	// some Monero daemon in addition to a Monero wallet would probably be overkill and too
	// much hassle: Assume the wallet to be fully synced and give ITS current height as a
	// "good-enough" value of current blockchain height. Give back a dummy hash because the
	// true hash of the current top block isn't available either of course.
	//
	// CAUTION, possible confusion ahead: The Monero wallet RCP command 'GetHeight' gives
	// back the current wallet height as it's commonly understood in the "Monero world",
	// which is "current number of blocks in the Monero blockchain", and not as it seems to
	// be more customary in the "Bitcoin world" as "height of the current top block". We thus
	// have to give back the result of 'GetHeight' MINUS ONE.
	//
	// Only debug log, gets called every 15 seconds
	w.lock()
	defer w.unlock()
	heightResp, err := w.client.GetHeight()
	if err != nil {
		w.logError(err)
		hash, _ := chainhash.NewHashFromStr("0")
		return 0, *hash
	}
	height := heightResp.Height
	height-- // See comment above
	// At least give each height a DIFFERENT dummy hash
	hash, _ := chainhash.NewHashFromStr(strconv.Itoa(int(height)))
	w.logDebug("Chain tip %d", height)
	return uint32(height), *hash
}

// Close the wallet
// This will stop the Monero RPC server as well.
func (w *Wallet) Close() {
	w.logInfo("")
	w.lock()
	defer w.unlock()
	w.client.StopWallet()
}

func feeLevelToPriorityAndMultiplier(feeLevel wi.FeeLevel) (walletrpc.Priority, int) {
	// The fee multipliers (second return value) are valid as of Monero release Boron Butterfly
	// in Spring 2019; there is no direct way to query them over wallet RPC.
	// The Monero wallet in its default configuration ("auto-low-priority=1" and "priority=0"
	// i.e. "default") implements a clever algorithm: If you use "default" as priority for your
	// transaction it checks whether already the lowest possible priority "unimportant" would bring
	// your transaction into the next block for sure (because currently blocks are not full), and
	// if yes uses that lowest priority, and if not uses priority "normal". So, to trigger that
	// mechanism, 'NORMAL' is translated to 'Default' here, and not 'Normal'.
	switch feeLevel {
	case wi.PRIOIRTY:
		// Don't use 'PriorityPriority' with its fee multiplier of 1000 as long as
		// there is no serious Monero network congestion whatsoever in sight
		return walletrpc.PriorityElevated, 25
	case wi.NORMAL:
		// Translate to 'PriorityDefault' and NOT to 'PriorityNormal', see comment above
		return walletrpc.PriorityDefault, 5
	case wi.ECONOMIC:
		return walletrpc.PriorityUnimportant, 1
	case wi.FEE_BUMP:
		// Monero has no fee bumps, give back something nevertheless
		return walletrpc.PriorityElevated, 25
	default:
		return walletrpc.PriorityNormal, 5
	}
}

func feeLevelToPriority(feeLevel wi.FeeLevel) walletrpc.Priority {
	priority, _ := feeLevelToPriorityAndMultiplier(feeLevel)
	return priority
}

func (w *Wallet) GetFeePerByte(feeLevel wi.FeeLevel) uint64 {
	// Typical XMR fee per byte value on Mainnet as of Spring 2019 with blocks mostly not full;
	// Exact value not available through the wallet RPC interface - see also comments
	// at 'EstimateFee' method
	w.logInfo("Get fee per byte for level %d", feeLevel)
	feePerByte := AmountToAtomicUnits(0.000000019459)
	_, multiplier := feeLevelToPriorityAndMultiplier(feeLevel)
	return uint64(feePerByte * int64(multiplier))
}

func (w *Wallet) Spend(amount int64, addr btc.Address, feeLevel wi.FeeLevel, referenceID string) (*chainhash.Hash, error) {
	w.logInfo("Spend %.12f to %s", AtomicUnitsToAmount(amount), addr.String())
	w.lock()
	defer w.unlock()
	dest := &walletrpc.Destination{Amount: uint64(amount), Address: addr.String()}
	req := walletrpc.RequestTransfer{Destinations: []*walletrpc.Destination{dest},
		Priority: feeLevelToPriority(feeLevel),
	}
	resp, err := w.client.Transfer(&req)
	if err != nil {
		w.logError(err)
		hash, _ := chainhash.NewHashFromStr("0")
		return hash, err
	}
	hash, _ := chainhash.NewHashFromStr(resp.TxHash)
	return hash, nil
}

func (w *Wallet) BumpFee(txid chainhash.Hash) (*chainhash.Hash, error) {
	w.logInfo("")
	err := errors.New("Monero does not support a fee-bump mechanism")
	w.logError(err)
	return nil, err
}

func (w *Wallet) EstimateFee(ins []wi.TransactionInput, outs []wi.TransactionOutput, feePerByte uint64) uint64 {
	// Since the introduction of "Bulletproofs" and the resulting drastically smaller
	// transactions Monero fees are typically a few US cents for most transactions using
	// the "Normal" priority, and sometimes not even a single cent with the "Unimportant"
	// priority, based on a price of around 70 USD for 1 XMR in Spring 2019. Furthermore,
	// Monero fees cannot be set to arbitrary values like with Bitcoin transactions,
	// and are therefore not subject to a "fee market" either where there is basically
	// no limit for the fees people pay to be sure to get into the next block when the
	// Bitcoin network is heavily congested: Monero fees are calculated automatically,
	// with only the priority left to the user to adjust.
	// Consequence of all this: Fee calculation for OpenBazaar using XMR is a lot less
	// important than for OpenBazaar using BTC. Which is nice because the mentioned
	// automatic fee calculation is so complicated, and support for calculating exact
	// fees over RPC call to the Monero wallet missing, that the calls here are
	// impossible to implement fully strictly according to specification.
	//
	// It's unclear who uses this method; the following method 'EstimateSpendFee' seems
	// to be far more important as it's called by the client when paying for orders.
	var size uint64
	inputs := len(ins)
	outputs := len(outs)
	w.logInfo("Estimate fee for %d inputs, %d outputs", inputs, outputs)
	if (inputs == 1) && (outputs == 2) {
		size = 1750
	} else if (inputs == 2) && (outputs == 2) {
		size = 2540
	} else {
		// Very rough estimate of transaction size: 0.85 kB per input, 0.18 kB per output
		size = uint64(inputs*850 + outputs*180)
	}
	return size * feePerByte
}

func (w *Wallet) EstimateSpendFee(amount int64, feeLevel wi.FeeLevel) (uint64, error) {
	// It seems there is no other simple way to estimate the fee than asking the wallet to actually
	// build a transaction, but without sending it to the network, and then ignore everything except the
	// fee that gets reported back.
	// Nice about this method: If with the current state of the wallet a lot of outputs are needed to
	// spend for reaching 'amount' and thus a particularly large transaction will result, this gets
	// correctly taken into account. Also the mechanism "use lowest possible fee if already that is
	// enough" (see comment at 'feeLevelToPriorityAndMultiplier') has a chance to trigger.
	// Negative consequence: If there is some problem preventing this "testing transaction" to be built
	// (locked funds or not enough funds) the call will fail, and fee estimation is not possible. (Which
	// is not as bad as it may sound: Mostly 'EstimateSpendFee' gets called before some spend action which
	// of course would not be possible either because of the SAME reasons that make fee estimation fail.)
	w.logInfo("Estimate spend fee amount %.12f, fee level %d", AtomicUnitsToAmount(amount), feeLevel)
	w.lock()
	defer w.unlock()
	if w.mainAddress == "" {
		// The RPC wallet was probably not yet online when the wallet was created;
		// try to get the main address needed here belatedly
		err := w.getMainAddress()
		if (err != nil) || (w.mainAddress == "") {
			return 0, err
		}
	}
	dest := &walletrpc.Destination{Amount: uint64(amount), Address: w.mainAddress}
	req := walletrpc.RequestTransfer{Destinations: []*walletrpc.Destination{dest},
		Priority: feeLevelToPriority(feeLevel), DoNotRelay: true,
	}
	resp, err := w.client.Transfer(&req)
	if err != nil {
		w.logError(err)
		return 0, err
	}
	w.logInfo("Fee %.12f", AtomicUnitsToAmount(int64(resp.Fee)))
	return resp.Fee, err
}

func (w *Wallet) GenerateMultisigScript(keys []hd.ExtendedKey, threshold int, timeout time.Duration, timeoutKey *hd.ExtendedKey) (addr btc.Address, redeemScript []byte, err error) {
	err = multisigNotImplementedError("GenerateMultisigScript")
	w.logError(err)
	return nil, nil, err
}

func (w *Wallet) SweepAddress(ins []wi.TransactionInput, address *btc.Address, key *hd.ExtendedKey, redeemScript *[]byte, feeLevel wi.FeeLevel) (*chainhash.Hash, error) {
	err := multisigNotImplementedError("SweepAddress")
	w.logError(err)
	return nil, err
}

func (w *Wallet) CreateMultisigSignature(ins []wi.TransactionInput, outs []wi.TransactionOutput, key *hd.ExtendedKey, redeemScript []byte, feePerByte uint64) ([]wi.Signature, error) {
	err := multisigNotImplementedError("CreateMultisigSignature")
	w.logError(err)
	return nil, err
}

func (w *Wallet) Multisign(ins []wi.TransactionInput, outs []wi.TransactionOutput, sigs1 []wi.Signature, sigs2 []wi.Signature, redeemScript []byte, feePerByte uint64, broadcast bool) ([]byte, error) {
	err := multisigNotImplementedError("Multisign")
	w.logError(err)
	return nil, err
}

func (w *Wallet) AddWatchedAddress(addr btc.Address) error {
	// Do nothing: It seems only script addresses are such addresses in need of watching through
	// this mechanism, and Monero does not have such addresses. (The implementation of this method
	// in the Ethernet wallet is empty as well.)
	w.logInfo("Add watched address %s", addr.String)
	return nil
}

func (w *Wallet) AddTransactionListener(callback func(wi.TransactionCallback)) {
	w.logInfo("")
	w.listeners = append(w.listeners, callback)
}

func (w *Wallet) ReSyncBlockchain(fromTime time.Time) {
	// The current Monero wallet RPC interface does not support scanning from a specific block height,
	// and turning a time into a block height probably isn't supported either. So, scan everything
	// (according to the blockchain height at wallet creation time).
	w.logInfo("Resync start")
	w.lock()
	defer w.unlock()
	w.client.RescanBlockchain()
	w.logInfo("Resync end")

	// Now set the report height for the listeners to the blockheight of the first found transaction.
	// Result: ALL existing transactions will be seen by the client, or seen again. This way
	// you can use ReSync to correct any problems with payments that the client missed somehow.
	txns, err := w.transactionsUnlocked()
	if err != nil {
		w.logError(err)
		return
	}
	if len(txns) > 0 {
		w.reportHeight = uint64(txns[0].Height)
		w.logInfo("Report height %d", w.reportHeight)
		req := walletrpc.RequestSetAttribute{Key: reportHeightAttribute, Value: strconv.Itoa(int(w.reportHeight))}
		err := w.client.SetAttribute(&req)
		if err != nil {
			w.logError(err)
			return
		}
	}
}

func (w *Wallet) GetConfirmations(txid chainhash.Hash) (confirms, atHeight uint32, err error) {
	w.logInfo("")
	txn, err := w.GetTransaction(txid)
	if err != nil {
		return 0, 0, err
	}
	return uint32(txn.Confirmations), uint32(txn.Height), nil
}

func (w *Wallet) ExchangeRates() wallet.ExchangeRates {
	w.logInfo("")
	return w.priceFetcher
}

// Find out from which height we have to report transactions to listeners
// (Of course we have to report also transactions in blocks that were added while we were
// not running, so we have to know up to where we reported last time before exiting.)
// As there is no place (yet) in the server database to store such an "arbitrary" single
// value on behalf of a wallet implementation use the Monero wallet "attribute" mechanism
// to store it in one of the wallet files itself.
func (w *Wallet) getInitialReportingHeight() error {
	w.lock()
	defer w.unlock()
	req := walletrpc.RequestGetAttribute{Key: reportHeightAttribute}
	resp, err := w.client.GetAttribute(&req)
	if err != nil {
		w.logError(err)
		return err
	}
	if resp.Value != "" {
		// Stored in the wallet from the last run
		storedHeight, err := strconv.ParseInt(resp.Value, 10, 64)
		if err != nil {
			w.logError(err)
			return err
		}
		w.reportHeight = uint64(storedHeight)
		w.logDebug("Stored report height %d", w.reportHeight)
		return nil
	}
	// Take current height and store it for next time
	heightResp, err := w.client.GetHeight()
	if err != nil {
		w.logError(err)
		return err
	}
	w.reportHeight = heightResp.Height
	w.logDebug("Chain tip %d as report height", w.reportHeight)
	setReq := walletrpc.RequestSetAttribute{Key: reportHeightAttribute, Value: strconv.Itoa(int(w.reportHeight))}
	err = w.client.SetAttribute(&setReq)
	if err != nil {
		w.logError(err)
		return err
	}
	return nil
}

func (w *Wallet) listenerThread() {
	for {
		if w.client != nil {
			if w.reportHeight == 0 {
				err := w.getInitialReportingHeight()
				if err != nil {
					w.logError(err)
					break
				}
			}
			// Caution: 'MinHeight' is exclusive
			req := walletrpc.RequestGetTransfers{In: true, Out: true,
				FilterByHeight: true, MinHeight: w.reportHeight - 1, MaxHeight: math.MaxUint64}

			w.lock()
			resp, err := w.client.GetTransfers(&req)
			w.unlock()
			if err != nil {
				w.logError(err)
				break
			}
			w.logDebug("Check for new txs gave %d in, %d out", len(resp.In), len(resp.Out))
			var maxTxHeight uint64
			for _, transfer := range resp.In {
				// For an "In" transfer the receiving (sub)address is in 'transfer.Address' and not
				// in a 'destination' entry
				output := wi.TransactionOutput{Address: NewMoneroAddress(transfer.Address),
					Value: int64(transfer.Amount)}
				info := wi.TransactionCallback{Txid: transfer.TxID, Outputs: []wi.TransactionOutput{output},
					Height: int32(transfer.Height), Timestamp: time.Unix(int64(transfer.Timestamp), 0),
					Value: int64(transfer.Amount), WatchOnly: false, BlockTime: time.Unix(int64(transfer.Timestamp), 0)}
				w.logDebug("Submit to listeners in tx %s", transfer.TxID)
				for _, callback := range w.listeners {
					callback(info)
				}
				if transfer.Height > maxTxHeight {
					maxTxHeight = transfer.Height
				}
			}
			for _, transfer := range resp.Out {
				// As a difference to the 'Transaction' wallet method here also "Out" amounts have to be
				// reported as positive
				// Unfortunately after a rescan destination addresses are no longer known so the 'destination'
				// array can be empty here. Fortunately the server database table 'txmetadata' often can fill
				// in the missing info for display in the client.
				address := ""
				if len(transfer.Destinations) > 0 {
					address = transfer.Destinations[0].Address
				}
				output := wi.TransactionOutput{Address: NewMoneroAddress(address),
					Value: int64(transfer.Amount)}
				info := wi.TransactionCallback{Txid: transfer.TxID, Outputs: []wi.TransactionOutput{output},
					Height: int32(transfer.Height), Timestamp: time.Unix(int64(transfer.Timestamp), 0),
					Value: int64(transfer.Amount), WatchOnly: false, BlockTime: time.Unix(int64(transfer.Timestamp), 0)}
				w.logDebug("Submit to listeners out tx %s", transfer.TxID)
				for _, callback := range w.listeners {
					callback(info)
				}
				if transfer.Height > maxTxHeight {
					maxTxHeight = transfer.Height
				}
			}
			if maxTxHeight > 0 {
				w.reportHeight = maxTxHeight + 1
				w.lock()
				req := walletrpc.RequestSetAttribute{Key: reportHeightAttribute, Value: strconv.Itoa(int(w.reportHeight))}
				err := w.client.SetAttribute(&req)
				w.unlock()
				if err != nil {
					w.logError(err)
					break
				}
			}
		}
		time.Sleep(time.Duration(15) * time.Second)
	}
}

func NewMoneroWallet(cfg config.CoinConfig, params *chaincfg.Params, proxy proxy.Dialer, logger logging.Backend) (*Wallet, error) {
	w := Wallet{}

	w.testnet = params.Name != chaincfg.MainNetParams.Name
	w.rpcAddress = defaultRpcAddress
	if value, found := cfg.Options["RPCAddress"].(string); found {
		w.rpcAddress = value
	}

	// To make analyzing and debugging problems with Monero as a new and quite different
	// coin easier there is some extensive logging to file and/or console implemented
	// that is configurable through config file > "wallets" > "XMR" > "WalletOptions"
	// To log EVERYTHING, use the following as "WalletOptions":
	// { "LogToFile": true, "FileLogLevel": "DEBUG", "LogToConsole": true, "ConsoleLogLevel": "DEBUG" }

	// File logging default: Do log to file, but only errors
	w.logToFile = true
	if value, found := cfg.Options["LogToFile"].(bool); found {
		w.logToFile = value
	}
	if w.logToFile {
		w.fileLog = logging.MustGetLogger("monero")
		backend := logging.AddModuleLevel(logger)
		w.fileLog.SetBackend(backend)
		w.fileLog.ExtraCalldepth = 1
		backend.SetLevel(logging.ERROR, "monero")
		if value, found := cfg.Options["FileLogLevel"].(string); found {
			level, err := logging.LogLevel(value)
			if err == nil {
				backend.SetLevel(level, "monero")
			}
		}
	}

	// Console logging default: Don't log anything to console
	if value, found := cfg.Options["LogToConsole"].(bool); found {
		w.logToConsole = value
	}
	if w.logToConsole {
		w.consoleLog = logging.MustGetLogger("monero")
		baseBackend := logging.NewLogBackend(os.Stdout, "", log.LstdFlags)
		format := logging.MustStringFormatter(
			`%{color:reset}%{color}%{time:15:04:05.000} [%{level}] [%{module}/%{shortfunc}] %{message}`,
		)
		backend := logging.AddModuleLevel(logging.NewBackendFormatter(baseBackend, format))
		w.consoleLog.SetBackend(backend)
		w.consoleLog.ExtraCalldepth = 1
		backend.SetLevel(logging.ERROR, "monero")
		if value, found := cfg.Options["ConsoleLogLevel"].(string); found {
			level, err := logging.LogLevel(value)
			if err == nil {
				backend.SetLevel(level, "monero")
			}
		}
	}

	w.priceFetcher = NewMoneroPriceFetcher(proxy)

	w.logInfo("New wallet for RPC address %s", w.rpcAddress)

	w.client = walletrpc.New(walletrpc.Config{Address: w.rpcAddress})
	err := w.getMainAddress()

	return &w, err
}
