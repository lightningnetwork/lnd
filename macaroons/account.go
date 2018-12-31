package macaroons

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/lnwire"
)

const (
	// AccountIDLen is the length of the ID that is generated as an
	// unique identifier of an account. It is 8 bytes long so guessing
	// is improbable but it's still not mistaken for a SHA256 hash.
	AccountIDLen = 8

	// OneTimeBalance represents an account that has an initial balance
	// that is used up when it is spent and is not replenished
	// automatically.
	OneTimeBalance AccountType = iota

	// PeriodicBalance represents an account that gets its balance
	// replenished after a certain amount of time has passed.
	PeriodicBalance
)

// AccountType is an enum-like type which denotes the possible account types
// that can be referenced in macaroons to keep track of user's balances.
type AccountType uint8

// AccountID is the type of an account's unique ID.
type AccountID [AccountIDLen]byte

var (
	// accountBucketName is the name of the bucket where all accounting
	// based balances are stored.
	accountBucketName = []byte("accounts")

	// ErrAccountBucketNotFound specifies that there is no bucket for the
	// accounts in the DB yet which can/should only happen if the account
	// store has been corrupted or was initialized incorrectly.
	ErrAccountBucketNotFound = fmt.Errorf("account bucket not found")

	// ErrMalformed is returned if a binary stored account cannot be
	// marshaled back.
	ErrMalformed = fmt.Errorf("malformed data")

	// ErrAccNotFound is returned if an account could not be found in the
	// local bolt DB.
	ErrAccNotFound = fmt.Errorf("account not found")

	// ErrAccExpired is returned if an account has an expiration date set
	// and that date is in the past.
	ErrAccExpired = fmt.Errorf("account has expired")

	// ErrAccBalanceInsufficient is returned if the amount required to
	// perform a certain action is larger than the current balance of the
	// account
	ErrAccBalanceInsufficient = fmt.Errorf("account balance insufficient")
)

// OffChainBalanceAccount holds all information that is needed to keep track
// of an user's off-chain account balance. This balance can only be spent
// by paying invoices.
type OffChainBalanceAccount struct {
	// ID is the randomly generated account identifier.
	ID AccountID

	// Type is the account type.
	Type AccountType

	// InitialBalance stores the initial balance and is never updated.
	InitialBalance lnwire.MilliSatoshi

	// CurrentBalance is the currently available balance of the account
	// that is updated every time an invoice is paid.
	CurrentBalance lnwire.MilliSatoshi

	// LastUpdate keeps track of the last time the balance of the account
	// was updated.
	LastUpdate time.Time

	// ExpirationDate is a specific date in the future after which the
	// account is marked as expired. Can be set to nil for accounts that
	// never expire.
	ExpirationDate time.Time
}

// Marshal returns the account marshaled into a format suitable for storage.
func (a *OffChainBalanceAccount) Marshal() ([]byte, error) {
	// The marshaled format for the the account is as follows:
	//   <ID><Type><InitialBalance><CurrentBalance><LastUpdate>
	//   <ExpirationDate>
	//
	// The time.Time type is 15 bytes long when binary marshaled.
	//
	// AccountIdLen + Type (1 byte) + InitialBalance (8 bytes) +
	// CurrentBalance (8 bytes) + LastUpdate (15 bytes) +
	// ExpirationDate (15 bytes) = AccountIdLen + 47 bytes = 55 bytes
	marshaled := make([]byte, AccountIDLen+47)

	b := marshaled
	copy(b[:AccountIDLen], a.ID[:])
	b = b[AccountIDLen:]
	b[0] = byte(a.Type)
	b = b[1:]
	binary.LittleEndian.PutUint64(b[:8], uint64(a.InitialBalance))
	b = b[8:]
	binary.LittleEndian.PutUint64(b[:8], uint64(a.CurrentBalance))
	b = b[8:]
	lastUpdateMarshaled, err := a.LastUpdate.MarshalBinary()
	if err != nil {
		return nil, err
	}
	copy(b[:15], lastUpdateMarshaled)
	b = b[15:]
	expirationDateMarshaled, err := a.ExpirationDate.MarshalBinary()
	if err != nil {
		return nil, err
	}
	copy(b[:15], expirationDateMarshaled)

	return marshaled, nil
}

// Unmarshal unmarshals the account from a binary format.
func (a *OffChainBalanceAccount) Unmarshal(marshaled []byte) error {
	// The marshaled format for the the account is as follows:
	//   <ID><Type><InitialBalance><CurrentBalance><LastUpdate>
	//   <ExpirationDate>
	//
	// The time.Time type is 15 bytes long when binary marshaled.
	//
	// AccountIdLen + Type (1 byte) + InitialBalance (8 bytes) +
	// CurrentBalance (8 bytes) + LastUpdate (15 bytes) +
	// ExpirationDate (15 bytes) = AccountIdLen + 47 bytes = 55 bytes
	if len(marshaled) != AccountIDLen+47 {
		return ErrMalformed
	}

	copy(a.ID[:], marshaled[:AccountIDLen])
	marshaled = marshaled[AccountIDLen:]
	a.Type = AccountType(marshaled[0])
	marshaled = marshaled[1:]
	a.InitialBalance = lnwire.MilliSatoshi(
		binary.LittleEndian.Uint64(marshaled[:8]),
	)
	marshaled = marshaled[8:]
	a.CurrentBalance = lnwire.MilliSatoshi(
		binary.LittleEndian.Uint64(marshaled[:8]),
	)
	marshaled = marshaled[8:]
	a.LastUpdate = time.Time{}
	if err := a.LastUpdate.UnmarshalBinary(marshaled[:15]); err != nil {
		return err
	}
	marshaled = marshaled[15:]
	a.ExpirationDate = time.Time{}
	if err := a.ExpirationDate.UnmarshalBinary(marshaled[:15]); err != nil {
		return err
	}

	return nil
}

// HasExpired returns true if the account has an expiration date set and that
// date is in the past.
func (a *OffChainBalanceAccount) HasExpired() bool {
	return !a.ExpirationDate.IsZero() && a.ExpirationDate.Before(time.Now())
}

// AccountStorage wraps the bolt DB that stores all accounts and their balances.
type AccountStorage struct {
	*bbolt.DB
}

// NewAccountStorage creates an AccountStorage instance and the corresponding
// bucket in the bolt DB if it does not exist yet.
func NewAccountStorage(db *bbolt.DB) (*AccountStorage, error) {
	// If the store's bucket doesn't exist, create it.
	err := db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(accountBucketName)
		return err
	})
	if err != nil {
		return nil, err
	}

	// Return the DB wrapped in a AccountStorage object.
	return &AccountStorage{db}, nil
}

// NewAccount creates a new OffChainBalanceAccount with the given balance and a
// randomly chosen ID.
func (s *AccountStorage) NewAccount(balance lnwire.MilliSatoshi,
	expirationDate time.Time) (*OffChainBalanceAccount, error) {

	if balance == 0 {
		return nil, fmt.Errorf("a new account cannot have balance of 0")
	}

	// First, create a new instance of an account. Currently only the type
	// OneTimeBalance is supported.
	account := &OffChainBalanceAccount{
		Type:           OneTimeBalance,
		InitialBalance: balance,
		CurrentBalance: balance,
		LastUpdate:     time.Now(),
		ExpirationDate: expirationDate,
	}
	id, err := s.uniqueRandomAccountID()
	if err != nil {
		return nil, err
	}
	account.ID = id

	// Try storing the account in the account database so we can keep track
	// of its balance.
	if err := s.storeAccount(account); err != nil {
		return nil, err
	}
	return account, nil
}

// storeAccount writes an account to the database, overwriting the existing one
// if it exists.
func (s *AccountStorage) storeAccount(account *OffChainBalanceAccount) error {
	return s.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(accountBucketName)
		if bucket == nil {
			return ErrAccountBucketNotFound
		}
		accountBinary, err := account.Marshal()
		if err != nil {
			return err
		}
		return bucket.Put(account.ID[:], accountBinary)
	})
}

// uniqueRandomAccountID generates a new random ID and makes sure it does not
// yet exist in the DB.
func (s *AccountStorage) uniqueRandomAccountID() (AccountID, error) {
	var newID = AccountID{}
	if _, err := rand.Read(newID[:]); err != nil {
		return newID, err
	}
	if _, err := s.GetAccount(newID); err != ErrAccNotFound {
		return s.uniqueRandomAccountID()
	}
	return newID, nil
}

// GetAccount retrieves an account from the bolt DB and unmarshals it. If the
// account cannot be found, then ErrAccNotFound is returned.
func (s *AccountStorage) GetAccount(
	id AccountID) (*OffChainBalanceAccount, error) {

	// Try looking up and reading the account by its ID from the local
	// bolt DB.
	var accountBinary []byte
	err := s.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(accountBucketName)
		if bucket == nil {
			return ErrAccountBucketNotFound
		}
		accountBinary = bucket.Get(id[:])
		if len(accountBinary) == 0 {
			return ErrAccNotFound
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Now try unmarshaling the account back from the binary format it was
	// stored in.
	account := &OffChainBalanceAccount{}
	if err := account.Unmarshal(accountBinary); err != nil {
		return nil, err
	}

	return account, nil
}

// GetAccounts retrieves all accounts from the bolt DB and unmarshals them.
func (s *AccountStorage) GetAccounts() ([]*OffChainBalanceAccount, error) {
	var accounts []*OffChainBalanceAccount
	err := s.View(func(tx *bbolt.Tx) error {
		// This function will be called in the ForEach and receive
		// the key and value of each account in the DB. The key, which
		// is also the ID is not used because it is also marshaled into
		// the value.
		readFn := func(_, v []byte) error {
			if v == nil {
				return nil
			}
			account := &OffChainBalanceAccount{}
			if err := account.Unmarshal(v); err != nil {
				return err
			}
			accounts = append(accounts, account)
			return nil
		}

		// We know the bucket should exist since it's created when
		// the account storage is initialized.
		return tx.Bucket(accountBucketName).ForEach(readFn)

	})
	if err != nil {
		return nil, err
	}

	return accounts, nil
}

// RemoveAccount finds an account by its ID and removes it from the DB.
func (s *AccountStorage) RemoveAccount(id AccountID) error {
	err := s.Update(func(tx *bbolt.Tx) error {
		// First see if account exists.
		bucket := tx.Bucket(accountBucketName)
		if bucket == nil {
			return ErrAccountBucketNotFound
		}
		account := bucket.Get(id[:])
		if len(account) == 0 {
			return ErrAccNotFound
		}

		// Then remove it, if it exists.
		return bucket.Delete(id[:])
	})
	return err
}

// CheckAccountBalance ensures an account is valid and has a balance equal to
// or larger than the amount that is required.
func (s *AccountStorage) CheckAccountBalance(id AccountID,
	requiredBalance lnwire.MilliSatoshi) error {

	// Check that the account exists, it hasn't expired and has sufficient
	// balance.
	account, err := s.GetAccount(id)
	if err != nil {
		return err
	}
	if account.HasExpired() {
		return ErrAccExpired
	}
	if account.CurrentBalance < requiredBalance {
		return ErrAccBalanceInsufficient
	}
	return nil
}

// ChargeAccount subtracts the given amount from an account's balance.
func (s *AccountStorage) ChargeAccount(id AccountID,
	amount lnwire.MilliSatoshi) error {

	// Check that the account exists and has sufficient balance.
	account, err := s.GetAccount(id)
	if err != nil {
		return err
	}
	if err := s.CheckAccountBalance(id, amount); err != nil {
		return err
	}

	// Update the account and store it in the database.
	account.CurrentBalance -= amount
	account.LastUpdate = time.Now()
	return s.storeAccount(account)
}

// Close closes the underlying database.
func (s *AccountStorage) Close() error {
	return s.DB.Close()
}
