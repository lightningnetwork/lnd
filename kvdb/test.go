package kvdb

import (
	"bytes"
	"fmt"
)

type KV struct {
	key string
	val string
}

func reverseKVs(a []KV) []KV {
	for i, j := 0, len(a)-1; i < j; i, j = i+1, j-1 {
		a[i], a[j] = a[j], a[i]
	}

	return a
}

// FillDB fills the passed db with the passed nested data. If a passed map value
// is string, then it'll inserted as map value, otherwise as a subbucket.
func FillDB(db Backend, data map[string]interface{}) error {
	return Update(db, func(tx RwTx) error {
		for key, val := range data {
			bucket, err := tx.CreateTopLevelBucket([]byte(key))
			if err != nil {
				return err
			}

			m, ok := val.(map[string]interface{})
			if !ok {
				return fmt.Errorf("invalid top level bucket: "+
					"%v", key)
			}

			if err := fillBucket(bucket, m); err != nil {
				return err
			}
		}

		return nil
	}, func() {})
}

func fillBucket(bucket RwBucket, data map[string]interface{}) error {
	for k, v := range data {
		switch value := v.(type) {

		// Key contains value.
		case string:
			err := bucket.Put([]byte(k), []byte(value))
			if err != nil {
				return err
			}

		// Key contains a sub-bucket.
		case map[string]interface{}:
			subBucket, err := bucket.CreateBucket([]byte(k))
			if err != nil {
				return err
			}

			if err := fillBucket(subBucket, value); err != nil {
				return err
			}

		default:
			return fmt.Errorf("invalid value type: %T, for key: %v",
				k, value)
		}
	}

	return nil
}

// VerifyDB verifies the database against the given data set.
func VerifyDB(db Backend, data map[string]interface{}) error {
	return View(db, func(tx RTx) error {
		for key, val := range data {
			bucket := tx.ReadBucket([]byte(key))
			if bucket == nil {
				return fmt.Errorf("top level bucket %v not "+
					"found", key)
			}

			m, ok := val.(map[string]interface{})
			if !ok {
				return fmt.Errorf("invalid top level bucket: "+
					"%v", key)
			}

			if err := verifyBucket(bucket, m); err != nil {
				return err
			}
		}

		return nil
	}, func() {})
}

func verifyBucket(bucket RBucket, data map[string]interface{}) error {
	for k, v := range data {
		switch value := v.(type) {

		// Key contains value.
		case string:
			dbVal := bucket.Get([]byte(k))
			if !bytes.Equal(dbVal, []byte(value)) {
				return fmt.Errorf("value mismatch. Key: %v, "+
					"val: %v, expected: %v", k, dbVal, value)
			}

			// Key contains a sub-bucket.
		case map[string]interface{}:
			subBucket := bucket.NestedReadBucket([]byte(k))
			if subBucket == nil {
				return fmt.Errorf("bucket %v not found", k)
			}

			err := verifyBucket(subBucket, value)
			if err != nil {
				return err
			}

		default:
			return fmt.Errorf("invalid value type: %T for key: %v",
				value, k)
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
		return fmt.Errorf("unexpected keys in database, got: %v, "+
			"expected: %v", keyCount, len(data))
	}

	return nil
}
