package macaroons

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"github.com/lightningnetwork/lnd/lntypes"
	"time"

	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/lnwire"
)

const (
	// AccountIDLen is the length of the ID that is generated as an
	// unique identifier of an account. It is 8 bytes long so guessing
	// is improbable but it's still not mistaken for a SHA256 hash.
	AccountIDLen = 8
)

// AccountType is an enum-like type which denotes the possible account types
// that can be referenced in macaroons to keep track of user's balances.
type AccountType uint8

const (
	// OneTimeBalance represents an account that has an initial balance
	// that is used up when it is spent and is not replenished
	// automatically.
	OneTimeBalance AccountType = iota

	// PeriodicBalance represents an account that gets its balance
	// replenished after a certain amount of time has passed.
	PeriodicBalance
)

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

	// Invoices is a list of all invoices that are associated with the
	// account.
	Invoices []*lntypes.Hash
	
	// Payments is a list of all payments that are associated with the
	// account
	Payments []*lntypes.Hash
}

// HasExpired returns true if the account has an expiration date set and that
// date is in the past.
func (a *OffChainBalanceAccount) HasExpired() bool {
	if a.ExpirationDate.Unix() == 0 {
		return false
	}
	return a.ExpirationDate.Before(time.Now())
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
	id, err := s.uniqueRandomAccountID()
	if err != nil {
		return nil, err
	}
	account := &OffChainBalanceAccount{
		ID:             id,
		Type:           OneTimeBalance,
		InitialBalance: balance,
		CurrentBalance: balance,
		ExpirationDate: expirationDate,
		Invoices:       make([]*lntypes.Hash, 0),
		Payments:       make([]*lntypes.Hash, 0),
	}

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
		account.LastUpdate = time.Now()
		accountBinary, err := serializeAccount(account)
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

	// Now try to deserialize the account back from the binary format it was
	// stored in.
	account, err := deserializeAccount(accountBinary)
	if err != nil {
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
			account, err := deserializeAccount(v)
			if err != nil {
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

// checkBalance ensures an account is valid and has a balance equal to
// or larger than the amount that is required.
func (s *AccountStorage) checkBalance(id AccountID,
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

// chargeAccount subtracts the given amount from an account's balance.
func (s *AccountStorage) chargeAccount(id AccountID, payment *lntypes.Hash,
	amount lnwire.MilliSatoshi) error {

	// Check that the account exists and has sufficient balance.
	account, err := s.GetAccount(id)
	if err != nil {
		return err
	}
	if err := s.checkBalance(id, amount); err != nil {
		return err
	}

	// Update the account and store it in the database.
	account.CurrentBalance -= amount
	account.Payments = append(account.Payments, payment)
	return s.storeAccount(account)
}

// addInvoice associates a generated invoice with the given account,
// making it possible for the account to be credited in case the invoice is
// paid.
func (s *AccountStorage) addInvoice(id AccountID,
	invoice *lntypes.Hash) error {

	account, err := s.GetAccount(id)
	if err != nil {
		return err
	}
	account.Invoices = append(account.Invoices, invoice)
	return s.storeAccount(account)
}

// CreditAccount checks if a paid invoice was associated with any existing
// account. If an account is found that is still valid, it gets the invoice
// amount credited.
func (s *AccountStorage) CreditAccount(paidInvoice lntypes.Hash,
	amount lnwire.MilliSatoshi) error {
	
	accounts, err := s.GetAccounts()
	if err != nil {
		return err
	}
	for _, account := range accounts {
		if account.HasExpired() {
			continue
		}
		for _, createdInvoice := range account.Invoices {
			if !bytes.Equal(createdInvoice[:], paidInvoice[:]) {
				continue
			}
			
			// If we get here, the current account has the invoice
			// associated with it that was just paid. Credit the
			// amount to the account and finish the search.
			account.CurrentBalance += amount
			account.Payments = append(
				account.Payments, &paidInvoice,
			)
			return s.storeAccount(account)
		}
	}
	return nil
}

func serializeAccount(account *OffChainBalanceAccount) ([]byte, error) {
	var w bytes.Buffer
	err := lnwire.WriteElements(
		&w, account.ID[:], uint8(account.Type), account.InitialBalance,
		account.CurrentBalance, uint64(account.LastUpdate.UnixNano()),
		uint64(account.ExpirationDate.UnixNano()),
		uint32(len(account.Invoices)), uint32(len(account.Payments)),
	)
	if err != nil {
		return nil, err
	}
	for _, invoiceHash := range account.Invoices {
		err := lnwire.WriteElement(&w, invoiceHash[:])
		if err != nil {
			return nil, err
		}
	}
	for _, paymentHash := range account.Payments {
		err := lnwire.WriteElement(&w, paymentHash[:])
		if err != nil {
			return nil, err
		}
	}
	return w.Bytes(), nil
}

func deserializeAccount(content []byte) (*OffChainBalanceAccount, error) {
	var (
		r              = bytes.NewReader(content)
		account        = &OffChainBalanceAccount{}
		accType        uint8
		lastUpdate     uint64
		expirationDate uint64
		numInvoices    uint32
		numPayments    uint32
	)
	err := lnwire.ReadElements(
		r, account.ID[:], &accType, &account.InitialBalance,
		&account.CurrentBalance, &lastUpdate, &expirationDate,
		&numInvoices, &numPayments,
	)
	if err != nil {
		return nil, err
	}
	account.Type = AccountType(accType)
	account.LastUpdate = time.Unix(0, int64(lastUpdate))
	account.ExpirationDate = time.Unix(0, int64(expirationDate))
	account.Invoices = make([]*lntypes.Hash, numInvoices)
	for i := uint32(0); i < numInvoices; i++ {
		var hash lntypes.Hash
		err := lnwire.ReadElement(r, hash[:])
		if err != nil {
			return nil, err
		}
		account.Invoices[i] = &hash
	}
	account.Payments = make([]*lntypes.Hash, numPayments)
	for i := uint32(0); i < numPayments; i++ {
		var hash lntypes.Hash
		err := lnwire.ReadElement(r, hash[:])
		if err != nil {
			return nil, err
		}
		account.Payments[i] = &hash
	}
	return account, nil
}
