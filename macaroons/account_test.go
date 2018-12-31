package macaroons_test

import (
	"bytes"
	"encoding/hex"
	"github.com/lightningnetwork/lnd/macaroons"
	"testing"
	"time"
)

var (
	testExpDateFuture = time.Date(
		time.Now().Year()+1, time.July, 2, 15, 44, 0, 0, time.UTC,
	)
	testExpDatePast = time.Date(
		time.Now().Year()-1, time.July, 2, 15, 44, 0, 0, time.UTC,
	)
)

// makeAccountID parses a hex string into an AccountID.
func makeAccountID(t *testing.T, id string) macaroons.AccountID {
	idBytes, err := hex.DecodeString(id)
	if err != nil {
		t.Fatalf("Error parsing ID %s as hex: %v", id, err)
	}
	var accountID macaroons.AccountID
	copy(accountID[:], idBytes)
	return accountID
}

// TestAccount tests the creation of a new account, reading/ deleting accounts
// and their marshaling/unmarshaling into/from the bolt DB.
func TestAccount(t *testing.T) {
	// First, initialize and unlock the service.
	service, cleanup := setupService(t)
	defer cleanup()

	// Now let's create a new account. It is automatically marshaled
	// and stored in the account bolt DB.
	account, err := service.NewAccount(9735, testExpDateFuture)
	if err != nil {
		t.Fatalf("Error creating account: %v", err)
	}

	// Fetch the same account from the bolt DB again and compare
	// it to the initial version.
	fetchedAccount, err := service.GetAccount(account.ID)
	if err != nil {
		t.Fatalf("Error fetching account: %v", err)
	}
	if !bytes.Equal(account.ID[:], fetchedAccount.ID[:]) {
		t.Fatalf(
			"Mismatched IDs. Expected %s, got %s.", account.ID,
			fetchedAccount.ID,
		)
	}
	if account.Type != fetchedAccount.Type {
		t.Fatalf(
			"Mismatched types. Expected %d, got %d.", account.Type,
			fetchedAccount.Type,
		)
	}
	if account.InitialBalance != fetchedAccount.InitialBalance {
		t.Fatalf(
			"Mismatched initial balances. Expected %d, got %d.",
			account.InitialBalance, fetchedAccount.InitialBalance,
		)
	}
	if account.CurrentBalance != fetchedAccount.CurrentBalance {
		t.Fatalf(
			"Mismatched current balances. Expected %d, got %d.",
			account.CurrentBalance, fetchedAccount.CurrentBalance,
		)
	}
	if !account.LastUpdate.Equal(fetchedAccount.LastUpdate) {
		t.Fatalf(
			"Mismatched last update. Expected %d, got %d.",
			account.LastUpdate.Unix(),
			fetchedAccount.LastUpdate.Unix(),
		)
	}
	if !account.ExpirationDate.Equal(fetchedAccount.ExpirationDate) {
		t.Fatalf(
			"Mismatched expiration date. Expected %d, got %d.",
			account.ExpirationDate.Unix(),
			fetchedAccount.ExpirationDate.Unix(),
		)
	}

	// Check that the account has not expired and that we can charge it with
	// an amount up to its balance.
	if account.HasExpired() {
		t.Fatalf("Account has expired but we didn't expect it to.")
	}
	if err := service.CheckAccountBalance(account.ID, 735); err != nil {
		t.Fatalf("Checking account balance failes: %v", err)
	}
	if err := service.ChargeAccount(account.ID, 735); err != nil {
		t.Fatalf("Error charging account: %v", err)
	}

	// Next, fetch all accounts and see if we also get the same account.
	accounts, err := service.GetAccounts()
	if err != nil {
		t.Fatalf("Error fetching accounts: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf(
			"Mismatched number of accounts. Expected %d, got %d.",
			1, len(accounts),
		)
	}
	if !bytes.Equal(accounts[0].ID[:], account.ID[:]) {
		t.Fatalf(
			"Mismatched IDs. Expected %s, got %s.", account.ID,
			fetchedAccount.ID,
		)
	}
	if accounts[0].CurrentBalance != 9000 {
		t.Fatalf(
			"Mismatched balance. Expected %d, got %d.", 9000,
			accounts[0].CurrentBalance,
		)
	}

	// Finally, delete the account again.
	err = service.RemoveAccount(fetchedAccount.ID)
	if err != nil {
		t.Fatalf("Error deleting account: %v", err)
	}
}

// TestAccountEdgeCases tests default values and bound checks.
func TestAccountEdgeCases(t *testing.T) {
	// First, initialize and unlock the service.
	service, cleanup := setupService(t)
	defer cleanup()

	// Make sure a new account cannot be initialized with a balance of 0.
	_, err := service.NewAccount(0, testExpDateFuture)
	if err == nil {
		t.Fatalf("Initial account balance of 0 should be refused.")
	}

	// Removing an account that doesn't exist should result in an error.
	err = service.RemoveAccount(makeAccountID(t, "abcd"))
	if err != macaroons.ErrAccNotFound {
		t.Fatalf("Wrong error returned when removing account: %v", err)
	}

	// An expired account shouldn't be able to be charged.
	expiredAccount, err := service.NewAccount(9735, testExpDatePast)
	if err != nil {
		t.Fatalf("Error creating expired account: %v", err)
	}
	err = service.CheckAccountBalance(expiredAccount.ID, 1)
	if err != macaroons.ErrAccExpired {
		t.Fatalf("Wrong error returned when checking account: %v", err)
	}
	err = service.ChargeAccount(expiredAccount.ID, 1)
	if err != macaroons.ErrAccExpired {
		t.Fatalf("Wrong error returned when charging account: %v", err)
	}

	// An account can't be over charged.
	smallAccount, err := service.NewAccount(500, testExpDateFuture)
	if err != nil {
		t.Fatalf("Error creating small account: %v", err)
	}
	err = service.CheckAccountBalance(smallAccount.ID, 1000)
	if err != macaroons.ErrAccBalanceInsufficient {
		t.Fatalf("Wrong error returned when checking account: %v", err)
	}
	err = service.ChargeAccount(smallAccount.ID, 1000)
	if err != macaroons.ErrAccBalanceInsufficient {
		t.Fatalf("Wrong error returned when charging account: %v", err)
	}
}
