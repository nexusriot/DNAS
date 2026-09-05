package core

import (
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// vaultSpend builds a spend of the vault at (hot, cold, unlock), signed by w —
// which must hold one of the two keys.
func vaultSpend(t *testing.T, w *wallet.Wallet, hot, cold string, unlock uint64, to string, amount, fee, nonce uint64) Transaction {
	t.Helper()
	addr, err := wallet.VaultAddress(hot, cold, unlock)
	if err != nil {
		t.Fatalf("vault address: %v", err)
	}
	tx := Transaction{
		From:   addr,
		To:     to,
		Amount: amount,
		Fee:    fee,
		Nonce:  nonce,
		Vault:  &VaultScript{Hot: hot, Cold: cold, Unlock: unlock},
	}
	tx.SignVault(w)
	return tx
}

// fundVault mines coin to a miner, matures it, and pays `amount` into the vault
// address, returning the miner wallet so the caller can keep mining.
func fundVault(t *testing.T, bc *Blockchain, vaultAddr string, amount uint64) *wallet.Wallet {
	t.Helper()
	miner, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	matureCoinbase(t, bc)
	fund := signedTx(t, miner, vaultAddr, amount, testFee, 0)
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), []Transaction{fund}))
	return miner
}

func TestVaultColdKeySpendsImmediately(t *testing.T) {
	withUpgrade(t, UpgradeVault)
	bc := NewBlockchain()
	hot, _ := wallet.New()
	cold, _ := wallet.New()
	addr, err := wallet.VaultAddress(hot.PublicKeyHex(), cold.PublicKeyHex(), 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	miner := fundVault(t, bc, addr, 10*testFee)
	// The unlock height is far away, so only the cold key can move this coin.
	spend := vaultSpend(t, cold, hot.PublicKeyHex(), cold.PublicKeyHex(), 1_000_000, miner.Address(), testFee, testFee, 0)
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), []Transaction{spend}))
	if got := bc.Balance(addr); got != 8*testFee {
		t.Fatalf("vault balance = %d, want %d", got, 8*testFee)
	}
}

func TestVaultHotKeyMustWaitForUnlock(t *testing.T) {
	withUpgrade(t, UpgradeVault)
	bc := NewBlockchain()
	hot, _ := wallet.New()
	cold, _ := wallet.New()
	unlock := uint64(1_000_000)
	addr, err := wallet.VaultAddress(hot.PublicKeyHex(), cold.PublicKeyHex(), unlock)
	if err != nil {
		t.Fatal(err)
	}
	miner := fundVault(t, bc, addr, 10*testFee)

	early := vaultSpend(t, hot, hot.PublicKeyHex(), cold.PublicKeyHex(), unlock, miner.Address(), testFee, testFee, 0)
	err = bc.AddBlock(mineOn(t, bc, miner.Address(), []Transaction{early}))
	if err == nil {
		t.Fatal("the hot key spent before the vault unlocked")
	}
	if !strings.Contains(err.Error(), "vault hot-key spend before unlock") {
		t.Fatalf("unexpected rejection: %v", err)
	}

	// The same spend is fine once the unlock height is behind us.
	past := vaultSpend(t, hot, hot.PublicKeyHex(), cold.PublicKeyHex(), 1, miner.Address(), testFee, testFee, 0)
	pastAddr := past.From
	fundMiner := signedTx(t, miner, pastAddr, 10*testFee, testFee, 1)
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), []Transaction{fundMiner}))
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), []Transaction{past}))
}

func TestVaultRejectsForeignSignature(t *testing.T) {
	withUpgrade(t, UpgradeVault)
	hot, _ := wallet.New()
	cold, _ := wallet.New()
	stranger, _ := wallet.New()
	spend := vaultSpend(t, stranger, hot.PublicKeyHex(), cold.PublicKeyHex(), 10, "dnasx", 1, 1, 0)
	if err := spend.VerifySignature(); err == nil {
		t.Fatal("a key that is in neither vault role authorized a spend")
	}
}

func TestVaultScriptMustMatchTheAddress(t *testing.T) {
	withUpgrade(t, UpgradeVault)
	hot, _ := wallet.New()
	cold, _ := wallet.New()
	spend := vaultSpend(t, cold, hot.PublicKeyHex(), cold.PublicKeyHex(), 10, "dnasx", 1, 1, 0)
	// Changing the unlock height changes the address the script hashes to, so the
	// spend no longer refers to the account it is draining.
	spend.Vault.Unlock = 11
	spend.SignVault(cold)
	if err := spend.VerifySignature(); err == nil {
		t.Fatal("a vault script that does not hash to the sender address was accepted")
	}
}

func TestVaultInactiveBeforeUpgrade(t *testing.T) {
	bc := NewBlockchain()
	hot, _ := wallet.New()
	cold, _ := wallet.New()
	addr, err := wallet.VaultAddress(hot.PublicKeyHex(), cold.PublicKeyHex(), 1)
	if err != nil {
		t.Fatal(err)
	}
	miner := fundVault(t, bc, addr, 10*testFee)
	spend := vaultSpend(t, cold, hot.PublicKeyHex(), cold.PublicKeyHex(), 1, miner.Address(), testFee, testFee, 0)
	err = bc.AddBlock(mineOn(t, bc, miner.Address(), []Transaction{spend}))
	if err == nil {
		t.Fatal("a vault spend was accepted before the upgrade activated")
	}
	if !strings.Contains(err.Error(), "vault spends are not active") {
		t.Fatalf("unexpected rejection: %v", err)
	}
}

func TestVaultAddressRequiresDistinctKeys(t *testing.T) {
	w, _ := wallet.New()
	if _, err := wallet.VaultAddress(w.PublicKeyHex(), w.PublicKeyHex(), 10); err == nil {
		t.Fatal("a vault with one key in both roles was allowed")
	}
}

// The miner must never select a hot-key spend that is not yet unlocked: the
// candidate block would be invalid by its own rules.
func TestMempoolSelectSkipsLockedVaultSpend(t *testing.T) {
	withUpgrade(t, UpgradeVault)
	bc := NewBlockchain()
	hot, _ := wallet.New()
	cold, _ := wallet.New()
	unlock := uint64(1_000_000)
	addr, err := wallet.VaultAddress(hot.PublicKeyHex(), cold.PublicKeyHex(), unlock)
	if err != nil {
		t.Fatal(err)
	}
	miner := fundVault(t, bc, addr, 10*testFee)
	mp := NewMempool().UseAccounts(bc)
	spend := vaultSpend(t, hot, hot.PublicKeyHex(), cold.PublicKeyHex(), unlock, miner.Address(), testFee, testFee, 0)
	if added, err := mp.Add(spend); !added || err != nil {
		t.Fatalf("mempool refused a valid (if not yet spendable) vault spend: added=%v err=%v", added, err)
	}
	if sel := mp.Select(bc, 10); len(sel) != 0 {
		t.Fatalf("selected %d locked vault spend(s); the block would be rejected", len(sel))
	}
}

// Nonce-jamming: a thief holding the hot key can park a spend the chain will not
// accept until the unlock height. It occupies the vault's nonce, and the cold
// key's rescue uses that same nonce — so unless an unmineable transaction can be
// displaced for free, the rescuer has to out-bid the thief to save their own
// coin. The mempool therefore lets a mineable transaction take the slot.
func TestVaultRescueDisplacesAJammedNonce(t *testing.T) {
	withUpgrade(t, UpgradeVault)
	bc := NewBlockchain()
	hot, _ := wallet.New()
	cold, _ := wallet.New()
	unlock := uint64(1_000_000)
	addr, err := wallet.VaultAddress(hot.PublicKeyHex(), cold.PublicKeyHex(), unlock)
	if err != nil {
		t.Fatal(err)
	}
	miner := fundVault(t, bc, addr, 10*testFee)
	mp := NewMempool().UseAccounts(bc)

	// The thief parks an expensive, unmineable hot-key spend at nonce 0.
	jam := vaultSpend(t, hot, hot.PublicKeyHex(), cold.PublicKeyHex(), unlock, hot.Address(), testFee, 5*testFee, 0)
	if added, err := mp.Add(jam); !added || err != nil {
		t.Fatalf("the jamming spend was not queued: added=%v err=%v", added, err)
	}

	// The rescue pays LESS than the thief and must still win, because the thief's
	// transaction cannot go in the next block and the rescue can.
	rescue := vaultSpend(t, cold, hot.PublicKeyHex(), cold.PublicKeyHex(), unlock, miner.Address(), testFee, testFee, 0)
	added, err := mp.Add(rescue)
	if err != nil || !added {
		t.Fatalf("the cold-key rescue was refused: added=%v err=%v", added, err)
	}
	if _, ok := mp.Get(jam.Hash()); ok {
		t.Fatal("the jamming transaction still occupies the nonce")
	}
	if _, ok := mp.Get(rescue.Hash()); !ok {
		t.Fatal("the rescue is not in the pool")
	}
	if sel := mp.Select(bc, 10); len(sel) != 1 || sel[0].Hash() != rescue.Hash() {
		t.Fatalf("selection = %d transaction(s), want just the rescue", len(sel))
	}
}

// The displacement rule is narrow: it must not let a cheaper MINEABLE
// transaction evict another mineable one, which would break replace-by-fee.
func TestMineableTransactionStillNeedsAHigherFee(t *testing.T) {
	bc := NewBlockchain()
	sender, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, sender.Address(), nil))
	matureCoinbase(t, bc)
	mp := NewMempool().UseAccounts(bc)

	first := signedTx(t, sender, "dnasx", 100, 5*testFee, 0)
	if added, err := mp.Add(first); !added || err != nil {
		t.Fatalf("first: added=%v err=%v", added, err)
	}
	cheaper := signedTx(t, sender, "dnasy", 100, testFee, 0)
	if added, err := mp.Add(cheaper); added || err == nil {
		t.Fatal("a cheaper mineable transaction replaced a mineable one")
	}
	if _, ok := mp.Get(first.Hash()); !ok {
		t.Fatal("the original was dropped anyway")
	}
}
