package htlcswitch

import (
	"github.com/go-errors/errors"
	"github.com/jessevdk/go-flags"
	"github.com/lightningnetwork/lnd/lnwallet"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"os"
	"path/filepath"
	"time"

	"encoding/hex"
	pb "github.com/ExchangeUnion/swap-resolver/hashresolver"
)

const (
	defaultConfigFilename = "resolve.conf"
	defaultServerAddress  = "127.0.0.1:8886"
	defaultTLS            = false
)

type config struct {
	TLS                bool `long:"TLS" description:"If TLS should be used or not"`
	CaFile             string
	ServerAddr         string `long:"serveraddr" description:"host and port of the resolver"`
	ServerHostOverride string
}

var (
	cfg = &config{
		TLS:                defaultTLS,
		CaFile:             "",
		ServerAddr:         defaultServerAddress,
		ServerHostOverride: "",
	}
)

func isResolverActive() bool {
	// first see if we have a configuration file at the working directory. If
	// we miss that, the resolver is not active

	// TODO: config options should eventually become part of LND's config file and
	// command line options. Once this is done we will replace the code below with
	// as simple check of resolver.active
	dir, err := os.Getwd()
	if err != nil {
		log.Errorf(err.Error())
		return false
	}

	defaultConfigFile := filepath.Join(dir, "..", defaultConfigFilename)

	// now, try to read the configration. If not valid, the resolver is not
	// active
	log.Debugf("reading configuration from %v", defaultConfigFile)

	err = flags.IniParse(defaultConfigFile, cfg)

	if err != nil {
		log.Errorf("failed to read resolver configuration file (%v) - %v", defaultConfigFile, err)
		return false
	}

	// if all is well - resolver is active
	return true
}

func connectResolver() (*grpc.ClientConn, pb.HashResolverClient, error) {
	var opts []grpc.DialOption
	// TODO: add TLS support
	if cfg.TLS {
		//if *caFile == "" {
		//	*caFile = testdata.Path("ca.pem")
		//}
		//	creds, err := credentials.NewClientTLSFromFile(*caFile, *serverHostOverride)
		//	if err != nil {
		//		log.Fatalf("Failed to create TLS credentials %v", err)
		//	}
		//	opts = append(opts, grpc.WithTransportCredentials(creds))
	} else {
		opts = append(opts, grpc.WithInsecure())
	}
	conn, err := grpc.Dial(cfg.ServerAddr, opts...)
	if err != nil {
		log.Errorf("failed to dial: %v", err)
		return nil, nil, errors.New("failed to open connection to resolver")
	}

	return conn, pb.NewHashResolverClient(conn), nil
}

func queryPreImage(pd *lnwallet.PaymentDescriptor, heightNow uint32) (*pb.ResolveResponse, error) {

	conn, client, err := connectResolver()
	if err != nil {
		return nil, err
	}

	defer conn.Close()

	log.Debugf("Getting pre-image for hash: %v %v for amount %v", pd.RHash, hex.EncodeToString(pd.RHash[:]), int64(pd.Amount))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp, err := client.ResolveHash(ctx, &pb.ResolveRequest{
		Hash:      hex.EncodeToString(pd.RHash[:]),
		Amount:    int64(pd.Amount),
		Timeout:   pd.Timeout,
		Heightnow: heightNow,
	})
	if err != nil {
		log.Errorf("%v.ResolveHash(_) = _, %v: ", client, err)
		return nil, err
	}
	log.Debugf("Got response from Resolver: %v \n", resp)
	return resp, nil
}

type resolutionData struct {
	pd            *lnwallet.PaymentDescriptor
	l             *channelLink
	obfuscator    ErrorEncrypter
	preimageArray [32]byte
	failed        bool
}

func asyncResolve(pd *lnwallet.PaymentDescriptor, l *channelLink, obfuscator ErrorEncrypter, heightNow uint32) {

	go func() {

		// prepare message to main routine
		resolution := resolutionData{
			pd:         pd,
			l:          l,
			obfuscator: obfuscator,
		}

		resp, err := queryPreImage(pd, heightNow)

		if err != nil {
			log.Errorf("Error from queryPreImage: %v", err)
			resolution.failed = true
			l.resolver <- resolution
			return
		}

		// we got a pre-image. Try to decode it
		preimage, err := hex.DecodeString(resp.Preimage)
		if err != nil {
			log.Errorf("unable to decode Preimage %v : "+
				" %v", resp.Preimage, err)
			resolution.failed = true
			l.resolver <- resolution
			return
		}

		copy(resolution.preimageArray[:], preimage[:32])
		log.Debugf("preimage %v , resp.Preimage %v, preimageArray %v", preimage, resp.Preimage, resolution.preimageArray)
		resolution.failed = false
		l.resolver <- resolution
	}()

}
