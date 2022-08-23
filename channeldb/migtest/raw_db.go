package migtest

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/lightningnetwork/lnd/kvdb"
)

// DumpDB dumps go code describing the contents of the database to stdout. This
// function is only intended for use during development.
//
// Example output:
//
//	map[string]interface{}{
//		hex("1234"): map[string]interface{}{
//			"human-readable": hex("102030"),
//			hex("1111"): hex("5783492373"),
//		},
//	}
func DumpDB(tx kvdb.RTx, rootKey []byte) error {
	bucket := tx.ReadBucket(rootKey)
	if bucket == nil {
		return fmt.Errorf("bucket %v not found", string(rootKey))
	}

	return dumpBucket(bucket)
}

func dumpBucket(bucket kvdb.RBucket) error {
	fmt.Printf("map[string]interface{} {\n")
	err := bucket.ForEach(func(k, v []byte) error {
		key := toString(k)
		fmt.Printf("%v: ", key)

		subBucket := bucket.NestedReadBucket(k)
		if subBucket != nil {
			err := dumpBucket(subBucket)
			if err != nil {
				return err
			}
		} else {
			fmt.Print(toHex(v))
		}
		fmt.Printf(",\n")

		return nil
	})
	if err != nil {
		return err
	}
	fmt.Printf("}")

	return nil
}

// RestoreDB primes the database with the given data set.
func RestoreDB(tx kvdb.RwTx, rootKey []byte, data map[string]interface{}) error {
	bucket, err := tx.CreateTopLevelBucket(rootKey)
	if err != nil {
		return err
	}

	return restoreDB(bucket, data)
}

func restoreDB(bucket kvdb.RwBucket, data map[string]interface{}) error {
	for k, v := range data {
		key := []byte(k)

		switch value := v.(type) {
		// Key contains value.
		case string:
			err := bucket.Put(key, []byte(value))
			if err != nil {
				return err
			}

		// Key contains a sub-bucket.
		case map[string]interface{}:
			subBucket, err := bucket.CreateBucket(key)
			if err != nil {
				return err
			}

			if err := restoreDB(subBucket, value); err != nil {
				return err
			}

		default:
			return errors.New("invalid type")
		}
	}

	return nil
}

// VerifyDB verifies the database against the given data set.
func VerifyDB(tx kvdb.RTx, rootKey []byte, data map[string]interface{}) error {
	bucket := tx.ReadBucket(rootKey)
	if bucket == nil {
		return fmt.Errorf("bucket %v not found", string(rootKey))
	}

	return verifyDB(bucket, data)
}

func verifyDB(bucket kvdb.RBucket, data map[string]interface{}) error {
	for k, v := range data {
		key := []byte(k)

		switch value := v.(type) {
		// Key contains value.
		case string:
			expectedValue := []byte(value)
			dbValue := bucket.Get(key)

			if !bytes.Equal(dbValue, expectedValue) {
				return errors.New("value mismatch")
			}

		// Key contains a sub-bucket.
		case map[string]interface{}:
			subBucket := bucket.NestedReadBucket(key)
			if subBucket == nil {
				return fmt.Errorf("bucket %v not found", k)
			}

			err := verifyDB(subBucket, value)
			if err != nil {
				return err
			}

		default:
			return errors.New("invalid type")
		}
	}

	keyCount := 0
	err := bucket.ForEach(func(k, v []byte) error {
		keyCount++
		return nil
	})
	if err != nil {
		return err
	}
	if keyCount != len(data) {
		return errors.New("unexpected keys in database")
	}

	return nil
}

func toHex(v []byte) string {
	if len(v) == 0 {
		return "nil"
	}

	return "hex(\"" + hex.EncodeToString(v) + "\")"
}

func toString(v []byte) string {
	readableChars := "abcdefghijklmnopqrstuvwxyz0123456789-"

	for _, c := range v {
		if !strings.Contains(readableChars, string(c)) {
			return toHex(v)
		}
	}

	return "\"" + string(v) + "\""
}

// Hex is a test helper function to convert readable hex arrays to raw byte
// strings.
func Hex(value string) string {
	b, err := hex.DecodeString(value)
	if err != nil {
		panic(err)
	}
	return string(b)
}
