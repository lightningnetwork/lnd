package lncfg

// RemoteSigner holds the configuration options for a remote RPC signer.
type RemoteSigner struct {
	Enable       bool   `long:"enable" description:"Use a remote signer for signing any on-chain related transactions or messages. Only recommended if local wallet is initialized as watch-only. Remote signer must use the same seed/root key as the local watch-only wallet but must have private keys."`
	RPCHost      string `long:"rpchost" description:"The remote signer's RPC host:port"`
	MacaroonPath string `long:"macaroonpath" description:"The macaroon to use for authenticating with the remote signer"`
	TLSCertPath  string `long:"tlscertpath" description:"The TLS certificate to use for establishing the remote signer's identity"`
}
