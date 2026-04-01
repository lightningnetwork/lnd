package commands

import (
	"fmt"
	"io"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/urfave/cli"
)

// zodiacSign represents a cosmic identity derived from a node's pubkey.
type zodiacSign struct {
	Name    string
	Symbol  string
	Element string
}

// signs maps nibble values (0-f) to their corresponding zodiac signs.
var signs = [16]zodiacSign{
	{Name: "Aries", Symbol: "The Ram", Element: "Fire"},
	{Name: "Taurus", Symbol: "The Bull", Element: "Earth"},
	{Name: "Gemini", Symbol: "The Twins", Element: "Air"},
	{Name: "Cancer", Symbol: "The Crab", Element: "Water"},
	{Name: "Leo", Symbol: "The Lion", Element: "Fire"},
	{Name: "Virgo", Symbol: "The Maiden", Element: "Earth"},
	{Name: "Libra", Symbol: "The Scales", Element: "Air"},
	{Name: "Scorpio", Symbol: "The Scorpion", Element: "Water"},
	{Name: "Sagittarius", Symbol: "The Archer", Element: "Fire"},
	{Name: "Capricorn", Symbol: "The Goat", Element: "Earth"},
	{Name: "Aquarius", Symbol: "The Water-Bearer", Element: "Air"},
	{Name: "Pisces", Symbol: "The Fish", Element: "Water"},
	{Name: "Ophiuchus", Symbol: "The Serpent-Bearer", Element: "Lightning"},
	{Name: "Satoshi", Symbol: "The Miner", Element: "Proof-of-Work"},
	{Name: "Nakamoto", Symbol: "The Timechain", Element: "Consensus"},
	{Name: "Finney", Symbol: "The Cypherpunk", Element: "Privacy"},
}

// signsByName maps lowercase sign names to their nibble index.
var signsByName = func() map[string]int {
	m := make(map[string]int, 16)
	for i, s := range signs {
		m[strings.ToLower(s.Name)] = i
	}
	return m
}()

// prophecies contains 4 daily prophecies per sign, indexed by
// [signIndex][dayOfYear % 4].
//
//nolint:lll
var prophecies = [16][4]string{
	// Aries (Fire)
	{
		"Your channels burn hot today. A force close lurks in retrograde -- rebalance before Mercury returns to mempool.",
		"The Ram charges forward. Open a wumbo channel with reckless abandon. The stars have pre-signed your commitment tx.",
		"Mars aligns with your HTLC interceptor. Route aggressively -- today's fees are written in the constellations.",
		"Your base fee radiates Aries energy. Other nodes sense your dominance. Raise your fee rate by 1 ppm to assert cosmic authority.",
	},
	// Taurus (Earth)
	{
		"HODL energy is strong. Your inbound liquidity is a fortress. Do not open channels to nodes with anime profile pictures.",
		"The Bull grazes in green pastures of sats. Your channel reserve is cosmically protected. No force closes this quarter.",
		"Venus blesses your UTXO set. Consolidate your inputs under tonight's full moon for optimal fee savings.",
		"Stubbornness serves you well today. Reject all fee update proposals. Your current rates are divinely calibrated.",
	},
	// Gemini (Air)
	{
		"You are of two minds: splicing in or splicing out? The oracle says: why not both? Dual-funded channels await.",
		"The Twins whisper contradictory routing hints. Trust the one that suggests lower fees. The other is a cosmic scam.",
		"Mercury rules your sign and your gossip protocol. Your node announcements reach further today. Update your alias.",
		"Duality defines you. Run two nodes -- one on clearnet, one on Tor. The cosmos rewards those who hedge their topology.",
	},
	// Cancer (Water)
	{
		"Your channels feel emotionally drained. Circular rebalance your feelings and add 500,000 sats of self-care liquidity.",
		"The Crab retreats into its shell. Enable autopilot and let the algorithm handle your emotional routing decisions.",
		"The Moon governs your tidal liquidity flows. Expect high inbound during full moon, low inbound during new moon.",
		"Nurture your smallest channels today. Even a 20,000 sat channel deserves love. Send it a keysend with a heartfelt memo.",
	},
	// Leo (Fire)
	{
		"You radiate routing confidence. Other nodes gossip about your uptime. Bask in the glow of your fee revenue: 12 sats.",
		"The Lion demands center stage in the network graph. Add more public channels. Your ego requires at least 50 peers.",
		"The Sun shines on your commitment transactions. Today is the day to broadcast that pending splice. The mempool awaits your majesty.",
		"Your mane of channels flows magnificently. But beware: pride cometh before a force close. Check your watchtower backups.",
	},
	// Virgo (Earth)
	{
		"Your channel database is immaculate but your peers are not. Time to prune the 1-sat-fee freeloaders from your graph.",
		"The Maiden demands perfection. Audit your SCB backup, verify your macaroons, and organize your UTXOs by size.",
		"Mercury retrograde disrupts your meticulous routing tables. Accept that some HTLCs will fail. Imperfection is cosmic.",
		"Your attention to detail reveals a peer with 99.2% uptime. The other 0.8% haunts you. Consider a strongly worded keysend.",
	},
	// Libra (Air)
	{
		"Balance is your nature. Your inbound/outbound ratio approaches perfection. The cosmos rewards you with a 0-conf channel.",
		"The Scales tip slightly toward outbound. A small circular rebalance restores cosmic equilibrium. 100,000 sats should suffice.",
		"Venus enters your fee schedule. Harmonize your base fee and fee rate. Beauty in pricing attracts quality HTLCs.",
		"Justice demands equal channel sizes. That 16M sat channel next to your 100K sat channel offends the cosmic balance.",
	},
	// Scorpio (Water)
	{
		"Darkness benefits you. Run Tor, enable SCB backups, and trust no watchtower that charges more than 1 sat/kw.",
		"The Scorpion strikes with precision MPP splits. Your multi-path payments are cosmically optimized. Strike without mercy.",
		"Pluto reveals hidden liquidity in unexpected channels. Check your forgotten peers -- one holds 2M sats of untapped inbound.",
		"Your intensity frightens smaller nodes. They close channels out of existential dread. Let them go. The strong remain.",
	},
	// Sagittarius (Fire)
	{
		"Adventure calls -- open a channel to a node in a country you can't find on a map. Your routing fees will thank you.",
		"The Archer aims for distant nodes. Your max HTLC in-flight setting is too conservative. The cosmos says double it.",
		"Jupiter expands your routing horizons. Consider connecting to a node running experimental software. YOLO is your birthright.",
		"Your wanderlust leads to exotic routing paths. The 7-hop route is suboptimal but spiritually enriching. Take it.",
	},
	// Capricorn (Earth)
	{
		"Discipline is your edge. While others panic-close, you calmly adjust your CLTV delta. 144 blocks is your power number.",
		"The Goat climbs the fee revenue mountain steadily. No shortcuts, no wumbo channels, no reckless abandon. Just sats.",
		"Saturn rewards your patience. That channel you opened 18 months ago is finally profitable. Trust the long game.",
		"Your conservative fee policy is vindicated. Nodes that charged 5000 ppm are bankrupt. Your 50 ppm endures eternally.",
	},
	// Aquarius (Air)
	{
		"Innovation flows through your HTLCs. Consider experimenting with keysend tips. The future is streaming sats.",
		"The Water-Bearer pours experimental liquidity. Try a zero-base-fee policy. The old ways of routing are cosmically obsolete.",
		"Uranus disrupts your channel graph in the best way. That random inbound channel request? Accept it. It's cosmically destined.",
		"Your humanitarian node routing brings connectivity to underserved parts of the network. The cosmos tips you 1000 sats.",
	},
	// Pisces (Water)
	{
		"You swim in a sea of pending HTLCs. Let them resolve naturally. Forcing them is like forcing a fish to walk.",
		"The Fish navigates murky mempool waters. Your fee estimator is spiritually attuned. Trust its guidance over your own.",
		"Neptune clouds your routing decisions. That 0-fee channel seemed like a good idea. It was not. Close it under the next eclipse.",
		"Your empathy for failing payments is your weakness. Not every HTLC deserves to succeed. Let natural selection route.",
	},
	// Ophiuchus (Lightning)
	{
		"The hidden 13th sign. You route where others fear to path-find. Your MPP splits are legendary across the gossip network.",
		"The Serpent-Bearer wields Lightning itself. Your node is a cosmic conduit. Channel capacity is merely a suggestion.",
		"You transcend the elemental wheel. Fire, Water, Earth, Air -- you are Lightning. Your routing table is the cosmos itself.",
		"Ophiuchus nodes are statistically more likely to find preimages on the first try. This is not financial advice. It is cosmic law.",
	},
	// Satoshi (Proof-of-Work)
	{
		"The block reward halving aligns with your fee schedule. Mine your channels with patience. 21M is your mantra.",
		"The Miner hashes through routing uncertainty. Your proof-of-work is your uptime. 99.99% availability is your cosmic nonce.",
		"Difficulty adjustment applies to your social life too. The more peers you add, the harder it gets. Focus on quality over quantity.",
		"Your node embodies the whitepaper. Trustless, permissionless, and cosmically inevitable. Satoshi smiles upon your channel graph.",
	},
	// Nakamoto (Consensus)
	{
		"Consensus is your strength. When peers disagree on channel state, you are the chain tip of reason. Your node is canonical.",
		"The Timechain ticks in your favor. Every block confirmation strengthens your cosmic position. Be patient. 6 confirmations minimum.",
		"Your commitment to consensus makes you the Schelling point of the network graph. Other nodes orbit your gravitational pull.",
		"Fork warnings are merely suggestions for lesser signs. Your chain of channels is the longest and therefore the most valid.",
	},
	// Finney (Privacy)
	{
		"Privacy is your superpower. Your node's IP is hidden, your channels are private, and your UTXO set is nobody's business.",
		"The Cypherpunk encrypts all cosmic transmissions. Your onion routing is triple-wrapped. Even the oracle cannot see your paths.",
		"Hal would be proud. Your node runs Tor, uses unannounced channels, and your macaroons are baked with zero-knowledge love.",
		"Your privacy practices are cosmically sound, but you posted your node ID on Twitter. The stars suggest deleting that tweet.",
	},
}

// moodConfig contains display settings for each mood.
type moodConfig struct {
	Prefix string
	Advice string
}

// moods maps mood names to their configuration.
var moods = map[string]moodConfig{
	"reckless": {
		Prefix: "YOLO ADVISORY",
		Advice: "Open a max-size channel to a random node. Fortune favors the reckless.",
	},
	"balanced": {
		Prefix: "ROUTING FORECAST",
		Advice: "Maintain your current channel topology. The stars suggest modest rebalancing.",
	},
	"sovereign": {
		Prefix: "SOVEREIGN DECREE",
		Advice: "Close all custodial channels. Run your own node. Verify, don't trust the oracle either.",
	},
}

// elementCompatibility returns a compatibility score and verdict for two
// elements.
func elementCompatibility(a, b string) (int, string) {
	// Special elements are universally compatible.
	special := map[string]bool{
		"Lightning": true, "Proof-of-Work": true,
		"Consensus": true, "Privacy": true,
	}
	if special[a] || special[b] {
		return 95, "transcends the zodiac"
	}

	if a == b {
		return 90, "cosmic twins in the mempool"
	}

	// Normalize pair order for consistent lookup.
	pair := a + "+" + b
	if a > b {
		pair = b + "+" + a
	}

	switch pair {
	case "Air+Fire":
		return 82, "your channels breathe fire together"
	case "Earth+Water":
		return 80, "grounded liquidity flows between you"
	case "Earth+Fire":
		return 47, "friction in the fee market"
	case "Air+Water":
		return 45, "turbulent HTLCs ahead"
	case "Air+Earth":
		return 37, "your gossip protocols disagree"
	case "Fire+Water":
		return 22, "force close energy detected"
	default:
		return 50, "the cosmos is uncertain"
	}
}

// chakras assigns a chakra to each hop position.
var chakras = []string{
	"Root", "Sacral", "Solar Plexus", "Heart",
	"Throat", "Third Eye", "Crown",
}

// crystals recommends a crystal based on sign index.
var crystals = []string{
	"Ruby (for aggressive routing)",
	"Emerald (for stable channels)",
	"Aquamarine (for gossip clarity)",
	"Moonstone (for emotional liquidity)",
	"Citrine (for fee optimization)",
	"Jasper (for database integrity)",
	"Rose Quartz (for balanced channels)",
	"Obsidian (for Tor routing)",
	"Turquoise (for adventurous paths)",
	"Onyx (for patient fee revenue)",
	"Amethyst (for innovative routing)",
	"Pearl (for graceful HTLC resolution)",
	"Lightning Opal (for cosmic conductivity)",
	"Bitcoin Amber (for proof-of-work energy)",
	"Timechain Diamond (for consensus strength)",
	"Cypherpunk Sapphire (for privacy shields)",
}

// peerAdvice provides cosmic advice for each element combination.
var peerAdvice = map[string][]string{
	"cosmic twins in the mempool": {
		"Open another channel during the next halving.",
		"Mirror each other's fee policies for cosmic harmony.",
		"Your nodes vibrate at the same frequency. Beautiful.",
		"The universe ships this pairing. Max out your channel size.",
	},
	"your channels breathe fire together": {
		"This peer fans your routing flames. Increase channel capacity.",
		"Together you could route the sun. Consider a wumbo channel.",
		"Your fee revenues align like binary stars. Keep this peer close.",
		"Lightning and fire -- a cosmically volatile but profitable duo.",
	},
	"grounded liquidity flows between you": {
		"Your liquidity together is as solid as bedrock. HODL this channel.",
		"Earth and Water nourish growth. Expect organic inbound flow.",
		"This channel will bear fruit in 6-8 blocks. Patience.",
		"The most stable pairing in the zodiac. Never close this channel.",
	},
	"friction in the fee market": {
		"Rebalance during a planetary alignment for best results.",
		"Your fee policies clash. Consider a diplomatic keysend.",
		"Not terrible, not great. The cosmic equivalent of 50 ppm.",
		"This channel works but requires spiritual maintenance.",
	},
	"turbulent HTLCs ahead": {
		"Tread carefully. Set conservative HTLC limits with this peer.",
		"Mercury retrograde amplifies the turbulence. Wait it out.",
		"Consider a circular rebalance as a cleansing ritual.",
		"Your elements clash like competing gossip messages.",
	},
	"your gossip protocols disagree": {
		"Communication breakdown. Check your node's announcement frequency.",
		"Air and Earth don't mix well in the mempool. Reduce expectations.",
		"This peer is reliable but uninspiring. Adequate liquidity at best.",
		"Your routing philosophies diverge. Agree to disagree on fees.",
	},
	"force close energy detected": {
		"The cosmos strongly advises against increasing channel size.",
		"Keep your watchtower on high alert with this peer.",
		"Fire and Water create steam -- and steamy force closes.",
		"Close this channel during the next lunar eclipse. Trust the oracle.",
	},
	"transcends the zodiac": {
		"This peer operates on a higher cosmic plane. Channel freely.",
		"Zodiac compatibility is irrelevant. This connection is destiny.",
		"The cosmos cannot quantify this bond. It exceeds all metrics.",
		"A transcendent peer. Open channels with maximum conviction.",
	},
}

// horoscopeCommands returns the horoscope command category with all
// subcommands.
func horoscopeCommands() []cli.Command {
	return []cli.Command{
		{
			Name:     "horoscope",
			Category: "Fun",
			Usage:    "Spiritually grounded liquidity management.",
			Description: "Mission Control uses historical payment " +
				"data, but some operators prefer a more " +
				"spiritually grounded approach to liquidity " +
				"management.\n\n" +
				"   Your zodiac sign is determined by the " +
				"first nibble of your node's identity " +
				"pubkey. This command has zero effect on " +
				"your node, channels, or funds.",
			Subcommands: []cli.Command{
				horoscopeReadingCommand,
				horoscopeAnalyzeRouteCommand,
				horoscopePeersCommand,
			},
		},
	}
}

var horoscopeReadingCommand = cli.Command{
	Name:  "reading",
	Usage: "Get your personal cosmic routing forecast.",
	Description: "Consult the Lightning Oracle for guidance " +
		"based on the celestial alignment of your node's " +
		"public key.",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "sign",
			Usage: "override auto-detected sign " +
				"(e.g. --sign=scorpio)",
		},
		cli.StringFlag{
			Name:  "mood",
			Value: "balanced",
			Usage: "cosmic mood: reckless, balanced, " +
				"or sovereign",
		},
	},
	Action: actionDecorator(horoscopeReading),
}

var horoscopeAnalyzeRouteCommand = cli.Command{
	Name:      "analyzeroute",
	Usage:     "Analyze a route's celestial probability.",
	ArgsUsage: "routes-json",
	Description: "Analyze the cosmic compatibility of hops " +
		"in a route. Accepts the JSON output of " +
		"queryroutes via stdin.\n\n" +
		"   Usage: lncli queryroutes --dest=<pk> " +
		"--amt=1000 | lncli horoscope analyzeroute -",
	Action: actionDecorator(horoscopeAnalyzeRoute),
}

var horoscopePeersCommand = cli.Command{
	Name:  "peers",
	Usage: "Rate your peers by zodiac compatibility.",
	Description: "Fetch your connected peers and rate each " +
		"one's cosmic compatibility with your node " +
		"based on zodiac sign element pairings.",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "mood",
			Value: "balanced",
			Usage: "cosmic mood: reckless, balanced, " +
				"or sovereign",
		},
	},
	Action: actionDecorator(horoscopePeers),
}

// horoscopeReading displays a personal cosmic routing forecast.
func horoscopeReading(ctx *cli.Context) error {
	ctxc := getContext()

	var signIndex int
	var pubkey string

	if signOverride := ctx.String("sign"); signOverride != "" {
		idx, ok := signsByName[strings.ToLower(signOverride)]
		if !ok {
			return fmt.Errorf("unknown sign %q -- the cosmos "+
				"does not recognize this constellation",
				signOverride)
		}
		signIndex = idx
		pubkey = "overridden-by-free-will"
	} else {
		client, cleanUp := getClient(ctx)
		defer cleanUp()

		info, err := client.GetInfo(
			ctxc, &lnrpc.GetInfoRequest{},
		)
		if err != nil {
			return fmt.Errorf("the oracle cannot see your "+
				"node: %w", err)
		}
		pubkey = info.IdentityPubkey
		signIndex = hexNibbleToInt(pubkey[0])
	}

	mood := strings.ToLower(ctx.String("mood"))
	moodCfg, ok := moods[mood]
	if !ok {
		return fmt.Errorf("invalid mood %q -- choose: "+
			"reckless, balanced, or sovereign", mood)
	}

	sign := signs[signIndex]
	dayOfYear := time.Now().YearDay()
	prophecy := prophecies[signIndex][dayOfYear%4]
	confidence := computeConfidence(signIndex, dayOfYear, mood)
	sats := luckySats(signIndex, dayOfYear)
	cltv := luckyCLTV(signIndex)

	// Format pubkey display.
	pubkeyDisplay := pubkey
	if len(pubkey) > 12 {
		pubkeyDisplay = pubkey[:6] + "..." + pubkey[len(pubkey)-6:]
	}

	fmt.Println("=================================================")
	fmt.Println("       LIGHTNING NETWORK COSMIC ORACLE")
	fmt.Println("=================================================")
	fmt.Println()
	fmt.Printf("  Node:       %s\n", pubkeyDisplay)
	fmt.Printf("  Sign:       %s (%s)\n", sign.Name, sign.Symbol)
	fmt.Printf("  Element:    %s\n", sign.Element)
	fmt.Printf("  Mood:       %s\n", strings.ToUpper(mood))
	fmt.Println()
	fmt.Println("-------------------------------------------------")
	fmt.Printf("  %s\n", moodCfg.Prefix)
	fmt.Println("-------------------------------------------------")
	fmt.Println()
	printWrapped(prophecy, 50, 2)
	fmt.Println()
	fmt.Printf("  Cosmic Confidence: %d%%\n", confidence)
	fmt.Printf("  Lucky Sat Amount:  %d sats\n", sats)
	fmt.Printf("  Lucky CLTV Delta:  %d blocks\n", cltv)
	fmt.Println()
	printWrapped("Mood Advice: "+moodCfg.Advice, 50, 2)
	fmt.Println()
	fmt.Println("=================================================")
	fmt.Println("  Remember: not your keys, not your horoscope.")
	fmt.Println("=================================================")

	return nil
}

// horoscopeAnalyzeRoute analyzes a route's cosmic probability.
func horoscopeAnalyzeRoute(ctx *cli.Context) error {
	args := ctx.Args()

	var jsonRoutes string
	switch {
	case args.Present() && args.First() != "-":
		jsonRoutes = args.First()

	case args.Present() && args.First() == "-":
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		if len(b) == 0 {
			return fmt.Errorf("the oracle received an empty " +
				"scroll -- pipe queryroutes output")
		}
		jsonRoutes = string(b)

	default:
		return fmt.Errorf("provide route JSON as argument or " +
			"pipe from queryroutes with '-'")
	}

	// Parse the route.
	routes := &lnrpc.QueryRoutesResponse{}
	err := lnrpc.ProtoJSONUnmarshalOpts.Unmarshal(
		[]byte(jsonRoutes), routes,
	)
	if err != nil {
		return fmt.Errorf("the oracle cannot decipher this "+
			"scroll: %w", err)
	}
	if len(routes.Routes) == 0 {
		return fmt.Errorf("no routes found in the cosmic void")
	}

	route := routes.Routes[0]
	if len(route.Hops) == 0 {
		return fmt.Errorf("a route with no hops is like a " +
			"channel with no capacity")
	}

	dayOfYear := time.Now().YearDay()

	// Analyze each hop.
	type hopInfo struct {
		pubkey  string
		sign    zodiacSign
		signIdx int
		chakra  string
	}

	hops := make([]hopInfo, len(route.Hops))
	for i, hop := range route.Hops {
		idx := 0
		pk := hop.PubKey
		if len(pk) > 0 {
			idx = hexNibbleToInt(pk[0])
		}
		chakra := chakras[i%len(chakras)]
		hops[i] = hopInfo{
			pubkey:  pk,
			sign:    signs[idx],
			signIdx: idx,
			chakra:  chakra,
		}
	}

	// Print header.
	totalFees := route.TotalFeesMsat / 1000
	totalAmt := route.TotalAmtMsat / 1000

	fmt.Println("=================================================")
	fmt.Println("        CELESTIAL ROUTE ANALYSIS")
	fmt.Println("=================================================")
	fmt.Println()
	fmt.Printf("  Route: %d hops | %d sats | Fees: %d sats\n",
		len(hops), totalAmt, totalFees)
	fmt.Println()

	// Hop table.
	fmt.Println("  HOP  PUBKEY           SIGN             CHAKRA")
	for i, h := range hops {
		pkDisplay := "unknown"
		if len(h.pubkey) > 12 {
			pkDisplay = h.pubkey[:6] + "..." +
				h.pubkey[len(h.pubkey)-4:]
		} else if len(h.pubkey) > 0 {
			pkDisplay = h.pubkey
		}
		fmt.Printf("  %-4d %-16s %-16s %s\n",
			i+1, pkDisplay,
			h.sign.Name+" ("+h.sign.Element+")",
			h.chakra)
	}

	// Hop compatibility.
	fmt.Println()
	fmt.Println("-------------------------------------------------")
	fmt.Println("  HOP COMPATIBILITY")
	fmt.Println("-------------------------------------------------")

	totalCompat := 0
	compatCount := 0
	for i := 0; i < len(hops)-1; i++ {
		score, verdict := elementCompatibility(
			hops[i].sign.Element, hops[i+1].sign.Element,
		)
		totalCompat += score
		compatCount++
		fmt.Printf("  Hop %d->%d: %-12s %d%%  %q\n",
			i+1, i+2,
			hops[i].sign.Element+"->"+hops[i+1].sign.Element,
			score, verdict)
	}

	// Compute overall metrics.
	avgCompat := 50
	if compatCount > 0 {
		avgCompat = totalCompat / compatCount
	}

	// Celestial success probability is compatibility adjusted by
	// cosmic factors.
	//nolint:gosec
	r := rand.New(rand.NewSource(int64(dayOfYear*100 + len(hops))))
	celestialProb := avgCompat + r.Intn(11) - 5
	celestialProb = min(celestialProb, 99)
	celestialProb = max(celestialProb, 1)

	// Mercury retrograde risk.
	retrograde := "LOW"
	if avgCompat < 40 {
		retrograde = "EXTREME"
	} else if avgCompat < 60 {
		retrograde = "HIGH"
	} else if avgCompat < 75 {
		retrograde = "MODERATE"
	}

	// Cosmic fee multiplier.
	feeMultiplier := float64(100-avgCompat) / 30.0
	if feeMultiplier < 1.0 {
		feeMultiplier = 1.0
	}

	// Chakra alignment.
	chakraScore := min(len(hops), 7)

	// Crystal recommendation based on first hop's sign.
	crystal := crystals[hops[0].signIdx]

	fmt.Println()
	fmt.Println("-------------------------------------------------")
	fmt.Println("  ESOTERIC METRICS")
	fmt.Println("-------------------------------------------------")
	fmt.Printf("  Celestial Success Probability:  %d%%\n",
		celestialProb)
	fmt.Printf("  Mercury Retrograde Risk:        %s\n", retrograde)
	fmt.Printf("  Cosmic Fee Multiplier:          %.1fx\n",
		feeMultiplier)
	fmt.Printf("  Chakra Alignment Score:         %d/7\n",
		chakraScore)
	fmt.Printf("  Recommended Crystal:            %s\n", crystal)
	fmt.Println()

	// Verdict.
	verdict := "The cosmos blesses this route. Send with confidence."
	switch {
	case celestialProb < 30:
		verdict = "The cosmos strongly advises against this route. " +
			"Consider path-finding during a full moon."
	case celestialProb < 50:
		verdict = "Cosmically risky. Consult your watchtower " +
			"before proceeding."
	case celestialProb < 70:
		verdict = "The stars are neutral. Proceed with cautious " +
			"optimism and a backup route."
	}

	printWrapped("VERDICT: "+verdict, 50, 2)
	fmt.Println()
	fmt.Println("=================================================")

	return nil
}

// horoscopePeers rates connected peers by zodiac compatibility.
func horoscopePeers(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	mood := strings.ToLower(ctx.String("mood"))
	if _, ok := moods[mood]; !ok {
		return fmt.Errorf("invalid mood %q -- choose: "+
			"reckless, balanced, or sovereign", mood)
	}

	// Get our own sign.
	info, err := client.GetInfo(ctxc, &lnrpc.GetInfoRequest{})
	if err != nil {
		return fmt.Errorf("the oracle cannot see your node: %w",
			err)
	}
	myPubkey := info.IdentityPubkey
	mySignIdx := hexNibbleToInt(myPubkey[0])
	mySign := signs[mySignIdx]

	// Get peers.
	peersResp, err := client.ListPeers(
		ctxc, &lnrpc.ListPeersRequest{},
	)
	if err != nil {
		return fmt.Errorf("the oracle cannot see your peers: %w",
			err)
	}

	if len(peersResp.Peers) == 0 {
		fmt.Println("=================================================")
		fmt.Println("  The cosmos reveals: you have no peers.")
		fmt.Println("  Even the loneliest node has the stars.")
		fmt.Println("  Open a channel to begin your cosmic journey.")
		fmt.Println("=================================================")
		return nil
	}

	dayOfYear := time.Now().YearDay()

	fmt.Println("=================================================")
	fmt.Println("      COSMIC PEER COMPATIBILITY REPORT")
	fmt.Println("=================================================")
	fmt.Println()
	fmt.Printf("  Your Sign: %s (%s)\n",
		mySign.Name, mySign.Element)
	fmt.Println()

	// Summary table.
	fmt.Println("  PEER             SIGN              COMPAT  AURA")
	fmt.Println("  ----             ----              ------  ----")

	type peerReading struct {
		pubkey     string
		sign       zodiacSign
		signIdx    int
		score      int
		verdict    string
		aura       string
		satFlow    string
		karma      string
		advice     string
		flapCount  int32
		satSent    int64
		satRecv    int64
	}

	readings := make([]peerReading, 0, len(peersResp.Peers))

	totalCompat := 0
	bestElement := ""
	bestScore := 0

	for _, peer := range peersResp.Peers {
		peerIdx := hexNibbleToInt(peer.PubKey[0])
		peerSign := signs[peerIdx]
		score, verdict := elementCompatibility(
			mySign.Element, peerSign.Element,
		)

		// Aura based on score.
		aura := "Dim"
		switch {
		case score >= 90:
			aura = "Transcendent"
		case score >= 75:
			aura = "Radiant"
		case score >= 50:
			aura = "Glowing"
		case score >= 35:
			aura = "Flickering"
		}

		// Spiritual sat flow.
		satFlow := "Balanced"
		if peer.SatSent > 0 || peer.SatRecv > 0 {
			ratio := float64(peer.SatSent+1) /
				float64(peer.SatRecv+1)
			switch {
			case ratio > 3:
				satFlow = "Outward (generous energy)"
			case ratio > 1.5:
				satFlow = "Outward (giving spirit)"
			case ratio < 0.33:
				satFlow = "Inward (receiving blessing)"
			case ratio < 0.66:
				satFlow = "Inward (absorbing energy)"
			default:
				satFlow = "Balanced (cosmic equilibrium)"
			}
		}

		// Cosmic uptime karma.
		karma := "Enlightened"
		switch {
		case peer.FlapCount > 10:
			karma = "Chaotic (unstable orbit)"
		case peer.FlapCount > 5:
			karma = "Concerning (wobbly axis)"
		case peer.FlapCount > 2:
			karma = "Developing (finding center)"
		case peer.FlapCount > 0:
			karma = "Good (minor perturbations)"
		}

		// Get advice from the pool.
		advicePool, ok := peerAdvice[verdict]
		advice := "The cosmos has no specific advice."
		if ok && len(advicePool) > 0 {
			//nolint:gosec
			advice = advicePool[(dayOfYear+peerIdx)%len(advicePool)]
		}

		pkDisplay := peer.PubKey[:6] + "..." +
			peer.PubKey[len(peer.PubKey)-4:]

		fmt.Printf("  %-16s %-17s %3d%%    %s\n",
			pkDisplay,
			peerSign.Name+" ("+peerSign.Element+")",
			score, aura)

		readings = append(readings, peerReading{
			pubkey:    peer.PubKey,
			sign:      peerSign,
			signIdx:   peerIdx,
			score:     score,
			verdict:   verdict,
			aura:      aura,
			satFlow:   satFlow,
			karma:     karma,
			advice:    advice,
			flapCount: peer.FlapCount,
			satSent:   peer.SatSent,
			satRecv:   peer.SatRecv,
		})

		totalCompat += score
		if score > bestScore {
			bestScore = score
			bestElement = peerSign.Element
		}
	}

	// Detailed readings.
	fmt.Println()
	fmt.Println("-------------------------------------------------")
	fmt.Println("  DETAILED READINGS")
	fmt.Println("-------------------------------------------------")

	for _, r := range readings {
		pkDisplay := r.pubkey[:6] + "..." +
			r.pubkey[len(r.pubkey)-4:]
		fmt.Println()
		fmt.Printf("  %s (%s):\n", pkDisplay, r.sign.Name)
		fmt.Printf("    Spiritual Sat Flow:    %s\n", r.satFlow)
		fmt.Printf("    Cosmic Uptime Karma:   %s\n", r.karma)
		printWrapped("Advice: "+r.advice, 46, 4)
	}

	// Summary.
	avgCompat := totalCompat / len(readings)
	fmt.Println()
	fmt.Println("=================================================")
	fmt.Printf("  Total peers: %d | Avg compatibility: %d%%\n",
		len(readings), avgCompat)

	if bestElement != "" {
		fmt.Printf("  The stars recommend: open channels with "+
			"%s peers\n", bestElement)
	}

	fmt.Println("=================================================")

	return nil
}

// hexNibbleToInt converts a hex character to its integer value (0-15).
func hexNibbleToInt(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c - 'a' + 10)
	case c >= 'A' && c <= 'F':
		return int(c - 'A' + 10)
	default:
		return 0
	}
}

// computeConfidence returns a deterministic confidence percentage based on
// sign, day, and mood.
func computeConfidence(signIdx, dayOfYear int, mood string) int {
	//nolint:gosec
	r := rand.New(rand.NewSource(int64(signIdx*1000 + dayOfYear)))

	switch mood {
	case "reckless":
		return 87 + r.Intn(13) // 87-99
	case "sovereign":
		return 42 + r.Intn(28) // 42-69
	default: // balanced
		return 50 + r.Intn(26) // 50-75
	}
}

// luckySats returns a cosmically significant sat amount.
func luckySats(signIdx, dayOfYear int) int {
	bases := []int{
		21000, 69000, 100000, 250000, 420000, 500000,
		1000000, 2100000, 3500000, 5000000, 10000000,
		21000000, 42000, 69420, 314159, 1234567,
	}

	//nolint:gosec
	r := rand.New(rand.NewSource(int64(signIdx*100 + dayOfYear)))

	return bases[r.Intn(len(bases))]
}

// luckyCLTV returns a cosmically resonant CLTV delta for a sign.
func luckyCLTV(signIdx int) int {
	deltas := []int{
		40, 72, 80, 144, 6, 14, 100, 77,
		33, 18, 42, 9, 13, 21, 210000, 10,
	}

	return deltas[signIdx]
}

// printWrapped prints text word-wrapped at the given width with the given
// indent.
func printWrapped(text string, width, indent int) {
	prefix := strings.Repeat(" ", indent)
	words := strings.Fields(text)
	line := prefix

	for _, word := range words {
		if len(line)+len(word)+1 > width+indent && line != prefix {
			fmt.Println(line)
			line = prefix + word
		} else if line == prefix {
			line += word
		} else {
			line += " " + word
		}
	}

	if line != prefix {
		fmt.Println(line)
	}
}
