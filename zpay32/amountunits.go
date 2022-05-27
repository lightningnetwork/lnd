package zpay32

import (
	"fmt"
	"github.com/lightningnetwork/lnd/omnicore"
	"strconv"

	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// toMSat is a map from a unit to a function that converts an amount
	// of that unit to millisatoshis.
	toMSat = map[byte]func(uint64) (lnwire.MilliSatoshi, error){
		'm': mBtcToMSat,
		'u': uBtcToMSat,
		'n': nBtcToMSat,
		'p': pBtcToMSat,
	}

	// fromMSat is a map from a unit to a function that converts an amount
	// in millisatoshis to an amount of that unit.
	fromMSat = map[byte]func(lnwire.MilliSatoshi) (uint64, error){
		'm': mSatToMBtc,
		'u': mSatToUBtc,
		'n': mSatToNBtc,
		'p': mSatToPBtc,
	}
)

// mBtcToMSat converts the given amount in milliBTC to millisatoshis.
func mBtcToMSat(m uint64) (lnwire.MilliSatoshi, error) {
	return lnwire.MilliSatoshi(m) * 100000000, nil
}

// uBtcToMSat converts the given amount in microBTC to millisatoshis.
func uBtcToMSat(u uint64) (lnwire.MilliSatoshi, error) {
	return lnwire.MilliSatoshi(u * 100000), nil
}

// nBtcToMSat converts the given amount in nanoBTC to millisatoshis.
func nBtcToMSat(n uint64) (lnwire.MilliSatoshi, error) {
	return lnwire.MilliSatoshi(n * 100), nil
}

// pBtcToMSat converts the given amount in picoBTC to millisatoshis.
func pBtcToMSat(p uint64) (lnwire.MilliSatoshi, error) {
	if p < 10 {
		return 0, fmt.Errorf("minimum amount is 10p")
	}
	if p%10 != 0 {
		return 0, fmt.Errorf("amount %d pBTC not expressible in msat",
			p)
	}
	return lnwire.MilliSatoshi(p / 10), nil
}

// mSatToMBtc converts the given amount in millisatoshis to milliBTC.
func mSatToMBtc(msat lnwire.MilliSatoshi) (uint64, error) {
	if msat%100000000 != 0 {
		return 0, fmt.Errorf("%d msat not expressible "+
			"in mBTC", msat)
	}
	return uint64(msat / 100000000), nil
}

// mSatToUBtc converts the given amount in millisatoshis to microBTC.
func mSatToUBtc(msat lnwire.MilliSatoshi) (uint64, error) {
	if msat%100000 != 0 {
		return 0, fmt.Errorf("%d msat not expressible "+
			"in uBTC", msat)
	}
	return uint64(msat / 100000), nil
}

// mSatToNBtc converts the given amount in millisatoshis to nanoBTC.
func mSatToNBtc(msat lnwire.MilliSatoshi) (uint64, error) {
	if msat%100 != 0 {
		return 0, fmt.Errorf("%d msat not expressible in nBTC", msat)
	}
	return uint64(msat / 100), nil
}

// mSatToPBtc converts the given amount in millisatoshis to picoBTC.
func mSatToPBtc(msat lnwire.MilliSatoshi) (uint64, error) {
	return uint64(msat * 10), nil
}

// decodeAmount returns the amount encoded by the provided string in
// millisatoshi.
func decodeAmount(amount string) (lnwire.MilliSatoshi, error) {
	if len(amount) < 1 {
		return 0, fmt.Errorf("amount must be non-empty")
	}

	// If last character is a digit, then the amount can just be
	// interpreted as BTC.
	char := amount[len(amount)-1]
	digit := char - '0'
	if digit >= 0 && digit <= 9 {
		btc, err := strconv.ParseUint(amount, 10, 64)
		if err != nil {
			return 0, err
		}
		return lnwire.MilliSatoshi(btc) * mSatPerBtc, nil
	}

	// If not a digit, it must be part of the known units.
	conv, ok := toMSat[char]
	if !ok {
		return 0, fmt.Errorf("unknown multiplier %c", char)
	}

	// Known unit.
	num := amount[:len(amount)-1]
	if len(num) < 1 {
		return 0, fmt.Errorf("number must be non-empty")
	}

	am, err := strconv.ParseUint(num, 10, 64)
	if err != nil {
		return 0, err
	}

	return conv(am)
}

// decodeAssetAmount returns the amount encoded by the provided string in
// omnicore.Amount.
func decodeAssetAmount(amount string) (omnicore.Amount, error) {
	if len(amount) < 1 {
		return 0, fmt.Errorf("amount must be non-empty")
	}

	// If last character is a digit, then the amount can just be
	// interpreted as ASSET.
	char := amount[len(amount)-1]
	digit := char - '0'
	if digit >= 0 && digit <= 9 {
		asset, err := strconv.ParseUint(amount, 10, 64)
		if err != nil {
			return 0, err
		}
		return omnicore.Amount(asset) * amtPerAsset, nil
	}

	// If not a digit, it must be part of the known units.
	conv, ok := toAmt[char]
	if !ok {
		return 0, fmt.Errorf("unknown multiplier %c", char)
	}

	// Known unit.
	num := amount[:len(amount)-1]
	if len(num) < 1 {
		return 0, fmt.Errorf("number must be non-empty")
	}

	am, err := strconv.ParseUint(num, 10, 64)
	if err != nil {
		return 0, err
	}

	return conv(am)
}
func decodeAmt(amount string,assetId uint32) (lnwire.UnitPrec11, error) {
	if assetId==0 || assetId==lnwire.BtcAssetId{
		res,err:=decodeAmount(amount)
		return lnwire.UnitPrec11(res),err
	}
	res,err:=decodeAssetAmount(amount)
	return lnwire.UnitPrec11(res),err
}


// encodeAmount encodes the provided millisatoshi amount using as few characters
// as possible.
func encodeAmount(msat lnwire.MilliSatoshi) (string, error) {
	// If possible to express in BTC, that will always be the shortest
	// representation.
	if msat%mSatPerBtc == 0 {
		return strconv.FormatInt(int64(msat/mSatPerBtc), 10), nil
	}

	// Should always be expressible in pico BTC.
	pico, err := fromMSat['p'](msat)
	if err != nil {
		return "", fmt.Errorf("unable to express %d msat as pBTC: %v",
			msat, err)
	}
	shortened := strconv.FormatUint(pico, 10) + "p"
	for unit, conv := range fromMSat {
		am, err := conv(msat)
		if err != nil {
			// Not expressible using this unit.
			continue
		}

		// Save the shortest found representation.
		str := strconv.FormatUint(am, 10) + string(unit)
		if len(str) < len(shortened) {
			shortened = str
		}
	}

	return shortened, nil
}


// encodeAssetAmount encodes the provided asset amount using as few characters
// as possible.
func encodeAssetAmount(aamt omnicore.Amount) (string, error) {
	// If possible to express in BTC, that will always be the shortest
	// representation.
	if aamt%amtPerAsset == 0 {
		return strconv.FormatInt(int64(aamt/amtPerAsset), 10), nil
	}

	// Should always be expressible in pico BTC.
	nano, err := fromAmt['n'](aamt)
	if err != nil {
		return "", fmt.Errorf("unable to express %d msat as nAsset: %v",
			aamt, err)
	}
	shortened := strconv.FormatUint(nano, 10) + "n"
	for unit, conv := range fromAmt {
		am, err := conv(aamt)
		if err != nil {
			// Not expressible using this unit.
			continue
		}

		// Save the shortest found representation.
		str := strconv.FormatUint(am, 10) + string(unit)
		if len(str) < len(shortened) {
			shortened = str
		}
	}

	return shortened, nil
}

func encodeAmt(amt lnwire.UnitPrec11,assetId uint32) (string, error) {
	if 	assetId==0 || assetId==lnwire.BtcAssetId{
		return encodeAmount(lnwire.MilliSatoshi(amt))
	}
	return encodeAssetAmount(omnicore.Amount(amt))
}


var (
	// toAmt is a map from a unit to a function that converts an amount
	// of that unit to omnicore.Amount.
	toAmt = map[byte]func(uint64) (omnicore.Amount, error){
		'm': mAssetToAmt,
		'u': uAssetToAmt,
		'n': nAssetToAmt,
	}

	// fromAmt is a map from a unit to a function that converts an amount
	// in omnicore.Amount to an amount of that unit.
	fromAmt = map[byte]func(omnicore.Amount) (uint64, error){
		'm': amtToMAsset,
		'u': amtToUAsset,
		'n': amtToNAsset,
	}
)


// mAssetToAmt converts the given amount in milliASSET to omnicore.Amount.
func mAssetToAmt(m uint64) (omnicore.Amount, error) {
	return omnicore.Amount(m * 100000), nil
}

// uAssetToAmt converts the given amount in nanoASSET to omnicore.Amount.
func uAssetToAmt(u uint64) (omnicore.Amount, error) {
	return omnicore.Amount(u * 100), nil
}

// nAssetToAmt converts the given amount in picoASSET to omnicore.Amount.
func nAssetToAmt(n uint64) (omnicore.Amount, error) {
	if n < 10 {
		return 0, fmt.Errorf("minimum amount is 10p")
	}
	if n%10 != 0 {
		return 0, fmt.Errorf("amount %d nASSET not expressible in omnicore.Amount",
			n)
	}
	return omnicore.Amount(n / 10), nil
}


// amtToMAsset converts the given amount in omnicore.Amount to milliASSET.
func amtToMAsset(msat omnicore.Amount) (uint64, error) {
	if msat%100000 != 0 {
		return 0, fmt.Errorf("%d msat not expressible "+
			"in mAsset", msat)
	}
	return uint64(msat / 100000), nil
}

// amtToUAsset converts the given amount in omnicore.Amount to microASSET.
func amtToUAsset(msat omnicore.Amount) (uint64, error) {
	if msat%100 != 0 {
		return 0, fmt.Errorf("%d msat not expressible in uAsset", msat)
	}
	return uint64(msat / 100), nil
}

// amtToNAsset converts the given amount in omnicore.Amount to nanoASSET.
func amtToNAsset(msat omnicore.Amount) (uint64, error) {
	return uint64(msat * 10), nil
}
