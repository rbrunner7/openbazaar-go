package monero

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"strings"

	exchange "github.com/OpenBazaar/spvwallet/exchangerates"
	"golang.org/x/net/proxy"
)

// The following implementation of a Monero exchange rate provider was shamelessly copied from
// one of the exchange rate providers of the other supported coins.

type ExchangeRateProvider struct {
	fetchUrl        string
	cache           map[string]float64
	client          *http.Client
	decoder         ExchangeRateDecoder
	bitcoinProvider *exchange.BitcoinPriceFetcher
}

type ExchangeRateDecoder interface {
	decode(dat interface{}, cache map[string]float64, bp *exchange.BitcoinPriceFetcher) (err error)
}

type KrakenDecoder struct{}
type BitfinexDecoder struct{}

type MoneroPriceFetcher struct {
	sync.Mutex
	cache     map[string]float64
	providers []*ExchangeRateProvider
}

func NewMoneroPriceFetcher(dialer proxy.Dialer) *MoneroPriceFetcher {
	bp := exchange.NewBitcoinPriceFetcher(dialer)
	z := MoneroPriceFetcher{
		cache: make(map[string]float64),
	}
	dial := net.Dial
	if dialer != nil {
		dial = dialer.Dial
	}
	tbTransport := &http.Transport{Dial: dial}
	client := &http.Client{Transport: tbTransport, Timeout: time.Minute}

	z.providers = []*ExchangeRateProvider{
		{"https://api.bitfinex.com/v1/pubticker/xmrbtc", z.cache, client, BitfinexDecoder{}, bp},
		{"https://api.kraken.com/0/public/Ticker?pair=XMRXBT", z.cache, client, KrakenDecoder{}, bp},
	}
	go z.run()
	return &z
}

func (z *MoneroPriceFetcher) GetExchangeRate(currencyCode string) (float64, error) {
	currencyCode = NormalizeCurrencyCode(currencyCode)

	z.Lock()
	defer z.Unlock()
	price, ok := z.cache[currencyCode]
	if !ok {
		return 0, errors.New("Currency not tracked")
	}
	return price, nil
}

func (z *MoneroPriceFetcher) GetLatestRate(currencyCode string) (float64, error) {
	currencyCode = NormalizeCurrencyCode(currencyCode)

	z.fetchCurrentRates()
	z.Lock()
	defer z.Unlock()
	price, ok := z.cache[currencyCode]
	if !ok {
		return 0, errors.New("Currency not tracked")
	}
	return price, nil
}

func (z *MoneroPriceFetcher) GetAllRates(cacheOK bool) (map[string]float64, error) {
	if !cacheOK {
		err := z.fetchCurrentRates()
		if err != nil {
			return nil, err
		}
	}
	z.Lock()
	defer z.Unlock()
	return z.cache, nil
}

func (z *MoneroPriceFetcher) UnitsPerCoin() int {
	return int(AmountToAtomicUnits(1.0))
}

func (z *MoneroPriceFetcher) fetchCurrentRates() error {
	z.Lock()
	defer z.Unlock()
	for _, provider := range z.providers {
		err := provider.fetch()
		if err == nil {
			return nil
		}
	}
	return errors.New("All exchange rate API queries failed")
}

func (z *MoneroPriceFetcher) run() {
	z.fetchCurrentRates()
	ticker := time.NewTicker(time.Minute * 15)
	for range ticker.C {
		z.fetchCurrentRates()
	}
}

func (provider *ExchangeRateProvider) fetch() (err error) {
	if len(provider.fetchUrl) == 0 {
		err = errors.New("Provider has no fetchUrl")
		return err
	}
	resp, err := provider.client.Get(provider.fetchUrl)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(resp.Body)
	var dataMap interface{}
	err = decoder.Decode(&dataMap)
	if err != nil {
		return err
	}
	return provider.decoder.decode(dataMap, provider.cache, provider.bitcoinProvider)
}

func (b KrakenDecoder) decode(dat interface{}, cache map[string]float64, bp *exchange.BitcoinPriceFetcher) (err error) {
	rates, err := bp.GetAllRates(false)
	if err != nil {
		return err
	}
	obj, ok := dat.(map[string]interface{})
	if !ok {
		return errors.New("KrackenDecoder type assertion failure")
	}
	result, ok := obj["result"]
	if !ok {
		return errors.New("KrakenDecoder: field `result` not found")
	}
	resultMap, ok := result.(map[string]interface{})
	if !ok {
		return errors.New("KrackenDecoder type assertion failure")
	}
	pair, ok := resultMap["XMRXBT"]
	if !ok {
		return errors.New("KrakenDecoder: field `XMRXBT` not found")
	}
	pairMap, ok := pair.(map[string]interface{})
	if !ok {
		return errors.New("KrackenDecoder type assertion failure")
	}
	c, ok := pairMap["c"]
	if !ok {
		return errors.New("KrakenDecoder: field `c` not found")
	}
	cList, ok := c.([]interface{})
	if !ok {
		return errors.New("KrackenDecoder type assertion failure")
	}
	rateStr, ok := cList[0].(string)
	if !ok {
		return errors.New("KrackenDecoder type assertion failure")
	}
	price, err := strconv.ParseFloat(rateStr, 64)
	if err != nil {
		return err
	}
	rate := price

	if rate == 0 {
		return errors.New("Bitcoin-Monero price data not available")
	}
	for k, v := range rates {
		cache[k] = v * rate
	}
	return nil
}

func (b BitfinexDecoder) decode(dat interface{}, cache map[string]float64, bp *exchange.BitcoinPriceFetcher) (err error) {
	rates, err := bp.GetAllRates(false)
	if err != nil {
		return err
	}
	obj, ok := dat.(map[string]interface{})
	if !ok {
		return errors.New("BitfinexDecoder type assertion failure")
	}
	r, ok := obj["last_price"]
	if !ok {
		return errors.New("BitfinexDecoder: field `last_price` not found")
	}
	rateStr, ok := r.(string)
	if !ok {
		return errors.New("BitfinexDecoder type assertion failure")
	}
	price, err := strconv.ParseFloat(rateStr, 64)
	if err != nil {
		return err
	}
	rate := price

	if rate == 0 {
		return errors.New("Bitcoin-Monero price data not available")
	}
	for k, v := range rates {
		cache[k] = v * rate
	}
	return nil
}

// NormalizeCurrencyCode standardizes the format for the given currency code
func NormalizeCurrencyCode(currencyCode string) string {
	return strings.ToUpper(currencyCode)
}
