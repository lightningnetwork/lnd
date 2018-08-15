// Copyright (c) 2013-2017 The btcsuite developers
// Copyright (c) 2015-2016 The Decred developers
// Copyright (C) 2015-2018 The Lightning Network Developers

package extensions

import (
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof" // extend net/http with pprof capability at /debug/pprof

	"github.com/lightningnetwork/lnd/config"
	"github.com/lightningnetwork/lnd/extpoints"
)

func init() {
	extpoints.RegisterExtension(new(HTTPProfiling), "httpprofiling")
}

// HTTPProfiling is an extension that sets up HTTP profiling for LND
type HTTPProfiling struct{}

// StartLnd enables the http profiling server if requested.
func (p *HTTPProfiling) StartLnd(cfg *config.Config) error {
	if cfg.Profile != "" {
		go func() {
			listenAddr := net.JoinHostPort("", cfg.Profile)
			profileRedirect := http.RedirectHandler("/debug/pprof",
				http.StatusSeeOther)
			http.Handle("/", profileRedirect)
			fmt.Println(http.ListenAndServe(listenAddr, nil))
		}()
	}
	return nil
}
