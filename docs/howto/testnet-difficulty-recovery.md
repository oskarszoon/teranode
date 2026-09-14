# Recovering an early testnet sync rejected by historical difficulty

Affected `v0.15.9-beta-2` nodes can stop at height 148 after rejecting canonical
block 149 (`00000000291d8e6f5d0d2a59de8f0f206917f3e00ff53edc8f6dcaddd61f3fe9`)
with `incorrect difficulty bits: got 1d00ffff, expected 1c7fff80`.
Descendants may also be stored as invalid. Upgrading the binary does not clear
those persisted verdicts.

The fix selects the original 2016-block retarget and testnet minimum-difficulty
recovery before DAA activation, with EDA enabled after UAHF. Proof-of-work and
checkpoint checks still apply. It does not grant checkpoint trust to legacy RPC
requests or automatically clear invalid records.

## Recovery

1. Stop ingestion and back up the affected node's blockchain, UTXO and blob stores
   together. Upgrade all services sharing those stores to the fixed version.
2. For a fresh installation stalled this early, start a new sync with fresh,
   isolated stores for **all** services. Keep the old stores as a backup until the
   new sync is verified; do not mix an empty blockchain store with old UTXO data.
3. If retaining the existing stores, restart the upgraded services and use the
   supported `reconsiderblock` RPC (or the configured CLI):

   ```bash
   teranode-cli reconsiderblock 00000000291d8e6f5d0d2a59de8f0f206917f3e00ff53edc8f6dcaddd61f3fe9
   ```

   This performs block validation and then reconsiders stored invalid children.
   Check its result and logs: missing block/subtree data or another invalid child
   can prevent completion. Do not clear database `invalid` flags by hand. If
   revalidation cannot complete with the retained data, use fresh isolated stores.
4. Verify the canonical best chain passes height 149, matches checkpoint 546
   (`000000002a936ca763904c3c35fce2f3556c559c0214345d31b1bcebf76acb70`), and
   advances beyond it. Confirm the original difficulty rejection and consequent
   invalid-parent/mined-set waits stop.
