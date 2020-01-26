package monero

import (
	"github.com/btcsuite/btcd/chaincfg"
)

// Monero address object satisfying the 'btc.Address' interface
// The implementation can be trivial because Monero knows no complications like Bitcoin's
// pay-to-pubkey (P2PK), pay-to-pubkey-hash (P2PKH), and pay-to-script-hash (P2SH)
// address types.
type Address struct {
	publicAddress string
}

func (a *Address) String() string {
	return a.publicAddress
}

func (a *Address) EncodeAddress() string {
	return a.publicAddress
}

// Get the raw script address bytes
// Part of (Bitcoin specific) multisig processing, not possible for Monero, thus panic to
// make sure we notice if it gets inadvertently called
func (a *Address) ScriptAddress() []byte {
	panic("No multisig scripting functionality for Monero")
}

// Check whether the address is for the indicated net
// Could be implemented by checking the first character of 'publicAddress, e.g. with all
// addresses starting with a "4" being Monero mainnet addresses, but it seems this call
// is not necessary / does not get called, so it's not implemented.
// Panic to make sure we notice if it gets inadvertently called
func (a *Address) IsForNet(net *chaincfg.Params) bool {
	panic("IsForNet not implemented for Monero addresses")
}

func NewMoneroAddress(p string) *Address {
	return &Address{publicAddress: p}
}
