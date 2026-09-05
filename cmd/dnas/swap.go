package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"strings"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// An on-chain asset-for-coin swap, planned from one command.
//
// The primitive already exists — an HTLC holds a native asset as happily as it
// holds coin — but using it for a trade means building TWO contracts that share
// one hash, with the right parties on each side and the right timeouts, and then
// running the four commands in the right order. Getting any of that wrong is how
// one side ends up able to take the money and walk. This turns the whole thing
// into one derivation and a printed script.
//
// The trade: ASSET-OWNER has an asset and wants coin; COIN-OWNER has coin and
// wants the asset.
//
//  1. asset-owner mints a preimage and shares only its hash (dnas htlc new);
//  2. asset-owner funds the ASSET leg, claimable by coin-owner with the preimage;
//  3. coin-owner funds the COIN leg, claimable by asset-owner with the preimage;
//  4. asset-owner claims the coin leg, which PUBLISHES the preimage on-chain;
//  5. coin-owner reads the preimage out of that transaction and claims the asset.
//
// If either side walks away, both legs refund after their timeouts and nobody
// loses anything but fees.

// swapPlan is a derived swap: the two contract addresses and the ordered steps
// that execute it.
type swapPlan struct {
	AssetContract string
	CoinContract  string
	AssetTimeout  uint64
	CoinTimeout   uint64
	Steps         []string
}

// swapFeeBudget is the coin each contract is funded with on top of its balance,
// so the spend that empties it can pay its own fee. Fees are always paid in coin,
// so an asset contract funded with only the asset is a contract nobody can spend.
const swapFeeBudget = core.DefaultMinRelayFee * 2000

// buildSwapPlan derives both legs of a swap. It is pure — no network, no keys —
// so the plan can be produced and checked offline by either party, and both
// parties derive byte-identical addresses from the same inputs.
//
// The timeouts must not be equal, and the COIN leg must expire FIRST. That is
// not a style preference: the asset owner is the one who reveals the preimage, so
// if their leg expired first they could sit on the secret, let the asset leg
// refund, and then still claim the coin. Ordering the timeouts the other way
// makes that impossible.
func buildSwapPlan(hash, assetOwnerPub, coinOwnerPub, assetID string, assetAmount, coinAmount, coinTimeout, assetTimeout uint64) (swapPlan, error) {
	switch {
	case strings.TrimSpace(hash) == "":
		return swapPlan{}, errors.New("a swap needs a hash (run `dnas htlc new` to mint one)")
	case assetID == "":
		return swapPlan{}, errors.New("-asset is required (which asset is being traded)")
	case assetAmount == 0 || coinAmount == 0:
		return swapPlan{}, errors.New("both sides of the trade must be non-zero")
	case coinTimeout == 0 || assetTimeout == 0:
		return swapPlan{}, errors.New("both timeouts are required")
	case coinTimeout >= assetTimeout:
		return swapPlan{}, fmt.Errorf(
			"the coin leg must time out BEFORE the asset leg (got coin %d, asset %d): otherwise the "+
				"party holding the preimage can let their own leg refund and still claim the other",
			coinTimeout, assetTimeout)
	}

	// Asset leg: funded by the asset owner, claimable by the coin owner.
	assetContract, err := wallet.HTLCAddress(hash, coinOwnerPub, assetOwnerPub, assetTimeout)
	if err != nil {
		return swapPlan{}, fmt.Errorf("asset leg: %w", err)
	}
	// Coin leg: funded by the coin owner, claimable by the asset owner.
	coinContract, err := wallet.HTLCAddress(hash, assetOwnerPub, coinOwnerPub, coinTimeout)
	if err != nil {
		return swapPlan{}, fmt.Errorf("coin leg: %w", err)
	}

	feeBudget := core.FormatAmount(swapFeeBudget)
	plan := swapPlan{
		AssetContract: assetContract,
		CoinContract:  coinContract,
		AssetTimeout:  assetTimeout,
		CoinTimeout:   coinTimeout,
		Steps: []string{
			fmt.Sprintf("[asset owner] fund the asset leg with the asset:\n"+
				"    dnas spv wallet -key ASSET_OWNER.json -asset %s send %s %d", assetID, assetContract, assetAmount),
			fmt.Sprintf("[asset owner] fund it with coin for its own claim fee (%s):\n"+
				"    dnas spv wallet -key ASSET_OWNER.json send %s %s", feeBudget, assetContract, feeBudget),
			fmt.Sprintf("[coin owner]  fund the coin leg (trade amount + claim fee):\n"+
				"    dnas spv wallet -key COIN_OWNER.json send %s %s",
				coinContract, core.FormatAmount(coinAmount+swapFeeBudget)),
			fmt.Sprintf("[asset owner] claim the coin leg, publishing the preimage:\n"+
				"    dnas htlc claim -wallet ASSET_OWNER.json -hash %s -sender %s -timeout %d -to ASSET_OWNER_ADDR",
				hash, coinOwnerPub, coinTimeout),
			fmt.Sprintf("[coin owner]  read the preimage out of that transaction, then claim the asset:\n"+
				"    dnas htlc claim -wallet COIN_OWNER.json -hash %s -sender %s -timeout %d -asset %s -to COIN_OWNER_ADDR -preimage REVEALED",
				hash, assetOwnerPub, assetTimeout, assetID),
			fmt.Sprintf("[either, if it stalls] refund after the timeouts (coin at %d, asset at %d):\n"+
				"    dnas htlc refund -wallet YOURS.json -hash %s -recipient COUNTERPARTY_PUBKEY -timeout T -to YOUR_ADDR [-asset %s]",
				coinTimeout, assetTimeout, hash, assetID),
		},
	}
	return plan, nil
}

// htlcSwap implements `dnas htlc swap`: derive both legs of an asset-for-coin
// swap and print the steps that execute it.
func htlcSwap(args []string) {
	fs := flag.NewFlagSet("htlc swap", flag.ExitOnError)
	hash := fs.String("hash", "", "sha256(preimage) hex, from `dnas htlc new` (the asset owner mints it)")
	assetOwner := fs.String("asset-owner", "", "public key of the party giving the asset")
	coinOwner := fs.String("coin-owner", "", "public key of the party giving the coin")
	assetID := fs.String("asset", "", "id of the asset being traded")
	assetAmount := fs.Uint64("asset-amount", 0, "how much of the asset changes hands")
	coinAmount := fs.String("coin-amount", "", "how much coin it is traded for, in DNAS")
	coinTimeout := fs.Uint64("coin-timeout", 0, "height at which the coin leg refunds (must be BEFORE the asset leg)")
	assetTimeout := fs.Uint64("asset-timeout", 0, "height at which the asset leg refunds")
	_ = fs.Parse(args)

	var coin uint64
	if strings.TrimSpace(*coinAmount) != "" {
		amount, err := core.ParseAmount(*coinAmount)
		if err != nil {
			log.Fatalf("bad -coin-amount: %v", err)
		}
		coin = amount
	}
	plan, err := buildSwapPlan(*hash, *assetOwner, *coinOwner, *assetID, *assetAmount, coin, *coinTimeout, *assetTimeout)
	if err != nil {
		log.Fatal(err)
	}
	printSwapPlan(plan, *assetID, *assetAmount, coin)
}

func printSwapPlan(plan swapPlan, assetID string, assetAmount, coinAmount uint64) {
	fmt.Printf("asset-for-coin swap: %d of %s  <->  %s\n\n", assetAmount, assetID, core.FormatAmount(coinAmount))
	fmt.Printf("asset leg  %s  (refunds at height %d)\n", plan.AssetContract, plan.AssetTimeout)
	fmt.Printf("coin leg   %s  (refunds at height %d)\n\n", plan.CoinContract, plan.CoinTimeout)
	fmt.Println("both parties should derive these addresses themselves and check they match")
	fmt.Println("before funding anything. Then, in order:")
	fmt.Println()
	for i, step := range plan.Steps {
		fmt.Printf("%d. %s\n\n", i+1, step)
	}
}
