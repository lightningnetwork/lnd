// Package labels contains labels used to label transactions broadcast by lnd.
// These labels are used across packages, so they are declared in a separate
// package to avoid dependency issues.
package labels

// External labels a transaction as user initiated via the api. This
// label is only used when a custom user provided label is not given.
const External = "external"
