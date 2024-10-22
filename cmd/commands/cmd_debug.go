package commands

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/andybalholm/brotli"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd"
	"github.com/lightningnetwork/lnd/lnencrypt"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/urfave/cli"
	"google.golang.org/protobuf/proto"
)

var getDebugInfoCommand = cli.Command{
	Name:     "getdebuginfo",
	Category: "Debug",
	Usage:    "Returns debug information related to the active daemon.",
	Action:   actionDecorator(getDebugInfo),
}

func getDebugInfo(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.GetDebugInfoRequest{}
	resp, err := client.GetDebugInfo(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

type DebugPackage struct {
	EphemeralPubKey  string `json:"ephemeral_public_key"`
	EncryptedPayload string `json:"encrypted_payload"`
}

var encryptDebugPackageCommand = cli.Command{
	Name:     "encryptdebugpackage",
	Category: "Debug",
	Usage:    "Collects a package of debug information and encrypts it.",
	Description: `
	When requesting support with lnd, it's often required to submit a lot of
	debug information to the developer in order to track down a problem.
	This command will collect all the relevant information and encrypt it
	using the provided public key. The resulting file can then be sent to
	the developer for further analysis.
	Because the file is encrypted, it is safe to send it over insecure
	channels or upload it to a GitHub issue.

	The file by default contains the output of the following commands:
	- lncli getinfo
	- lncli getdebuginfo
	- lncli getnetworkinfo

	By specifying the following flags, additional information can be added
	to the file (usually this will be requested by the developer depending
	on the issue at hand):
		--peers:
			- lncli listpeers
		--onchain:
			- lncli listunspent
			- lncli listchaintxns
		--channels:
			- lncli listchannels
			- lncli pendingchannels
			- lncli closedchannels

	Use 'lncli encryptdebugpackage 0xxxxxx... > package.txt' to write the
	encrypted package to a file called package.txt.
	`,
	ArgsUsage: "pubkey [--output_file F]",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "pubkey",
			Usage: "the public key to encrypt the information " +
				"for (hex-encoded, e.g. 02aabb..), this " +
				"should be provided to you by the issue " +
				"tracker or developer you're requesting " +
				"support from",
		},
		cli.StringFlag{
			Name: "output_file",
			Usage: "(optional) the file to write the encrypted " +
				"package to; if not specified, the debug " +
				"package is printed to stdout",
		},
		cli.BoolFlag{
			Name: "peers",
			Usage: "include information about connected peers " +
				"(lncli listpeers)",
		},
		cli.BoolFlag{
			Name: "onchain",
			Usage: "include information about on-chain " +
				"transactions (lncli listunspent, " +
				"lncli listchaintxns)",
		},
		cli.BoolFlag{
			Name: "channels",
			Usage: "include information about channels " +
				"(lncli listchannels, lncli pendingchannels, " +
				"lncli closedchannels)",
		},
	},
	Action: actionDecorator(encryptDebugPackage),
}

func encryptDebugPackage(ctx *cli.Context) error {
	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		return cli.ShowCommandHelp(ctx, "encryptdebugpackage")
	}

	var (
		args        = ctx.Args()
		pubKeyBytes []byte
		err         error
	)
	switch {
	case ctx.IsSet("pubkey"):
		pubKeyBytes, err = hex.DecodeString(ctx.String("pubkey"))
	case args.Present():
		pubKeyBytes, err = hex.DecodeString(args.First())
	}
	if err != nil {
		return fmt.Errorf("unable to decode pubkey argument: %w", err)
	}

	pubKey, err := btcec.ParsePubKey(pubKeyBytes)
	if err != nil {
		return fmt.Errorf("unable to parse pubkey: %w", err)
	}

	// Collect the information we want to send from the daemon.
	payload, err := collectDebugPackageInfo(ctx)
	if err != nil {
		return fmt.Errorf("unable to collect debug package "+
			"information: %w", err)
	}

	// We've collected the information we want to send, but before
	// encrypting it, we want to compress it as much as possible to reduce
	// the size of the final payload.
	var (
		compressBuf bytes.Buffer
		options     = brotli.WriterOptions{
			Quality: brotli.BestCompression,
		}
		writer = brotli.NewWriterOptions(&compressBuf, options)
	)
	_, err = writer.Write(payload)
	if err != nil {
		return fmt.Errorf("unable to compress payload: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("unable to compress payload: %w", err)
	}

	// Now we have the full payload that we want to encrypt, so we'll create
	// an ephemeral keypair to encrypt the payload with.
	localKey, err := btcec.NewPrivateKey()
	if err != nil {
		return fmt.Errorf("unable to generate local key: %w", err)
	}

	enc, err := lnencrypt.ECDHEncrypter(localKey, pubKey)
	if err != nil {
		return fmt.Errorf("unable to create encrypter: %w", err)
	}

	var cipherBuf bytes.Buffer
	err = enc.EncryptPayloadToWriter(compressBuf.Bytes(), &cipherBuf)
	if err != nil {
		return fmt.Errorf("unable to encrypt payload: %w", err)
	}

	response := DebugPackage{
		EphemeralPubKey: hex.EncodeToString(
			localKey.PubKey().SerializeCompressed(),
		),
		EncryptedPayload: hex.EncodeToString(
			cipherBuf.Bytes(),
		),
	}

	// If the user specified an output file, we'll write the encrypted
	// payload to that file.
	if ctx.IsSet("output_file") {
		fileName := lnd.CleanAndExpandPath(ctx.String("output_file"))
		jsonBytes, err := json.Marshal(response)
		if err != nil {
			return fmt.Errorf("unable to encode JSON: %w", err)
		}

		return os.WriteFile(fileName, jsonBytes, 0644)
	}

	// Finally, we'll print out the final payload as a JSON if no output
	// file was specified.
	printJSON(response)

	return nil
}

// collectDebugPackageInfo collects the information we want to send to the
// developer(s) from the daemon.
func collectDebugPackageInfo(ctx *cli.Context) ([]byte, error) {
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	info, err := client.GetInfo(ctxc, &lnrpc.GetInfoRequest{})
	if err != nil {
		return nil, fmt.Errorf("error getting info: %w", err)
	}

	debugInfo, err := client.GetDebugInfo(
		ctxc, &lnrpc.GetDebugInfoRequest{},
	)
	if err != nil {
		return nil, fmt.Errorf("error getting debug info: %w", err)
	}

	networkInfo, err := client.GetNetworkInfo(
		ctxc, &lnrpc.NetworkInfoRequest{},
	)
	if err != nil {
		return nil, fmt.Errorf("error getting network info: %w", err)
	}

	var payloadBuf bytes.Buffer
	addToBuf := func(msgs ...proto.Message) error {
		for _, msg := range msgs {
			jsonBytes, err := lnrpc.ProtoJSONMarshalOpts.Marshal(
				msg,
			)
			if err != nil {
				return fmt.Errorf("error encoding response: %w",
					err)
			}

			payloadBuf.Write(jsonBytes)
		}

		return nil
	}

	if err := addToBuf(info); err != nil {
		return nil, err
	}
	if err := addToBuf(debugInfo); err != nil {
		return nil, err
	}
	if err := addToBuf(info, debugInfo, networkInfo); err != nil {
		return nil, err
	}

	// Add optional information to the payload.
	if ctx.Bool("peers") {
		peers, err := client.ListPeers(ctxc, &lnrpc.ListPeersRequest{
			LatestError: true,
		})
		if err != nil {
			return nil, fmt.Errorf("error getting peers: %w", err)
		}
		if err := addToBuf(peers); err != nil {
			return nil, err
		}
	}

	if ctx.Bool("onchain") {
		unspent, err := client.ListUnspent(
			ctxc, &lnrpc.ListUnspentRequest{
				MaxConfs: math.MaxInt32,
			},
		)
		if err != nil {
			return nil, fmt.Errorf("error getting unspent: %w", err)
		}
		chainTxns, err := client.GetTransactions(
			ctxc, &lnrpc.GetTransactionsRequest{},
		)
		if err != nil {
			return nil, fmt.Errorf("error getting chain txns: %w",
				err)
		}
		if err := addToBuf(unspent, chainTxns); err != nil {
			return nil, err
		}
	}

	if ctx.Bool("channels") {
		channels, err := client.ListChannels(
			ctxc, &lnrpc.ListChannelsRequest{},
		)
		if err != nil {
			return nil, fmt.Errorf("error getting channels: %w",
				err)
		}
		pendingChannels, err := client.PendingChannels(
			ctxc, &lnrpc.PendingChannelsRequest{},
		)
		if err != nil {
			return nil, fmt.Errorf("error getting pending "+
				"channels: %w", err)
		}
		closedChannels, err := client.ClosedChannels(
			ctxc, &lnrpc.ClosedChannelsRequest{},
		)
		if err != nil {
			return nil, fmt.Errorf("error getting closed "+
				"channels: %w", err)
		}
		if err := addToBuf(
			channels, pendingChannels, closedChannels,
		); err != nil {
			return nil, err
		}
	}

	return payloadBuf.Bytes(), nil
}

var decryptDebugPackageCommand = cli.Command{
	Name:     "decryptdebugpackage",
	Category: "Debug",
	Usage:    "Decrypts a package of debug information.",
	Description: `
	Decrypt a debug package that was created with the encryptdebugpackage
	command. Decryption requires the private key that corresponds to the
	public key the package was encrypted to.
	The command expects the encrypted package JSON to be provided on stdin.
	If decryption is successful, the information will be printed to stdout.

	Use 'lncli decryptdebugpackage 0xxxxxx... < package.txt > decrypted.txt'
	to read the encrypted package from a file called package.txt and to
	write the decrypted content to a file called decrypted.txt.
	`,
	ArgsUsage: "privkey [--input_file F]",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "privkey",
			Usage: "the hex encoded private key to decrypt the " +
				"debug package",
		},
		cli.StringFlag{
			Name: "input_file",
			Usage: "(optional) the file to read the encrypted " +
				"package from; if not specified, the debug " +
				"package is read from stdin",
		},
	},
	Action: actionDecorator(decryptDebugPackage),
}

func decryptDebugPackage(ctx *cli.Context) error {
	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		return cli.ShowCommandHelp(ctx, "decryptdebugpackage")
	}

	var (
		args         = ctx.Args()
		privKeyBytes []byte
		err          error
	)
	switch {
	case ctx.IsSet("pubkey"):
		privKeyBytes, err = hex.DecodeString(ctx.String("pubkey"))
	case args.Present():
		privKeyBytes, err = hex.DecodeString(args.First())
	}
	if err != nil {
		return fmt.Errorf("unable to decode privkey argument: %w", err)
	}

	privKey, _ := btcec.PrivKeyFromBytes(privKeyBytes)

	// Read the file from stdin and decode the JSON into a DebugPackage.
	var pkg DebugPackage
	if ctx.IsSet("input_file") {
		fileName := lnd.CleanAndExpandPath(ctx.String("input_file"))
		jsonBytes, err := os.ReadFile(fileName)
		if err != nil {
			return fmt.Errorf("unable to read file '%s': %w",
				fileName, err)
		}

		err = json.Unmarshal(jsonBytes, &pkg)
		if err != nil {
			return fmt.Errorf("unable to decode JSON: %w", err)
		}
	} else {
		err = json.NewDecoder(os.Stdin).Decode(&pkg)
		if err != nil {
			return fmt.Errorf("unable to decode JSON: %w", err)
		}
	}

	// Decode the ephemeral public key and encrypted payload.
	ephemeralPubKeyBytes, err := hex.DecodeString(pkg.EphemeralPubKey)
	if err != nil {
		return fmt.Errorf("unable to decode ephemeral public key: %w",
			err)
	}
	encryptedPayloadBytes, err := hex.DecodeString(pkg.EncryptedPayload)
	if err != nil {
		return fmt.Errorf("unable to decode encrypted payload: %w", err)
	}

	// Parse the ephemeral public key and create an encrypter.
	ephemeralPubKey, err := btcec.ParsePubKey(ephemeralPubKeyBytes)
	if err != nil {
		return fmt.Errorf("unable to parse ephemeral public key: %w",
			err)
	}
	enc, err := lnencrypt.ECDHEncrypter(privKey, ephemeralPubKey)
	if err != nil {
		return fmt.Errorf("unable to create encrypter: %w", err)
	}

	// Decrypt the payload.
	decryptedPayload, err := enc.DecryptPayloadFromReader(
		bytes.NewReader(encryptedPayloadBytes),
	)
	if err != nil {
		return fmt.Errorf("unable to decrypt payload: %w", err)
	}

	// Decompress the payload.
	reader := brotli.NewReader(bytes.NewBuffer(decryptedPayload))
	decompressedPayload, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("unable to decompress payload: %w", err)
	}

	fmt.Println(string(decompressedPayload))

	return nil
}
