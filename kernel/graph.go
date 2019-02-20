package kernel

import (
	"fmt"
	"time"

	"github.com/MixinNetwork/mixin/common"
	"github.com/MixinNetwork/mixin/config"
	"github.com/MixinNetwork/mixin/crypto"
)

func (node *Node) handleSnapshotInput(s *common.Snapshot) error {
	if !s.NodeId.HasValue() {
		s.NodeId = node.IdForNetwork
	}

	retry, err := node.verifyTransactionInSnapshot(s)
	if retry {
		node.store.QueueAppendSnapshot(node.IdForNetwork, s)
	}
	if err != nil {
		return nil
	}

	err = node.tryToSignSnapshot(s)
	if err != nil {
		return err
	}

	retry, err = node.verifySnapshot(s)
	if retry {
		node.store.QueueAppendSnapshot(node.IdForNetwork, s)
	}
	if err != nil {
		return nil
	}

	defer node.Graph.UpdateFinalCache()
	if node.verifyFinalization(node.SnapshotsPool[s.Hash]) {
		return nil
	}
	s.Signatures = []crypto.Signature{node.SignaturesPool[s.Hash]}
	if node.IdForNetwork != s.NodeId {
		// FIXME gossip peers are different from consensus nodes
		return node.Peer.SendSnapshotMessage(s.NodeId, s, 0)
	}

	for peerId, _ := range node.ConsensusNodes {
		cacheId := s.Hash.ForNetwork(peerId)
		if time.Now().Before(node.ConsensusCache[cacheId].Add(time.Duration(config.SnapshotRoundGap * 2))) {
			continue
		}
		err = node.Peer.SendSnapshotMessage(peerId, s, 0)
		if err != nil {
			return err
		}
		node.ConsensusCache[cacheId] = time.Now()
	}
	return nil
}

func (node *Node) verifySnapshot(s *common.Snapshot) (bool, error) {
	s.Hash = s.PayloadHash()
	osigs := node.SnapshotsPool[s.Hash]
	if s.NodeId == node.IdForNetwork && len(osigs) == 0 {
		return false, fmt.Errorf("some node is impersonating me %s %s", node.IdForNetwork.String(), s.NodeId.String())
	}

	cache := node.Graph.CacheRound[s.NodeId].Copy()
	final := node.Graph.FinalRound[s.NodeId].Copy()

	if s.RoundNumber < cache.Number || s.RoundNumber > cache.Number+1 {
		return true, fmt.Errorf("invalid round number %d %d", s.RoundNumber, cache.Number)
	}
	if s.RoundNumber == cache.Number {
		if s.References[0] != cache.References[0] || s.References[1] != cache.References[1] {
			return true, fmt.Errorf("invalid same round references %s %s", cache.References[0].String(), cache.References[1].String())
		}
	} else if s.RoundNumber == cache.Number+1 {
		round, err := node.verifyReferences(s, cache)
		if err != nil {
			return true, err
		}
		if round == nil {
			return true, fmt.Errorf("invalid new round references %s %s", s.References[0].String(), s.References[1].String())
		}
		final = round
		cache = &CacheRound{
			NodeId:     s.NodeId,
			Number:     s.RoundNumber,
			Timestamp:  s.Timestamp,
			References: s.References,
		}
		err = node.store.StartNewRound(cache.NodeId, cache.Number, cache.References, final.Start)
		if err != nil {
			return true, err
		}
	}

	if len(osigs) > 0 {
		filter := make(map[crypto.Signature]bool)
		for _, sig := range osigs {
			filter[sig] = true
		}
		for _, sig := range s.Signatures {
			if filter[sig] {
				continue
			}
			osigs = append(osigs, sig)
			filter[sig] = true
		}
		node.SnapshotsPool[s.Hash] = append([]crypto.Signature{}, osigs...)
	} else {
		node.signSnapshot(s)
	}
	osigs = node.SnapshotsPool[s.Hash]
	if node.verifyFinalization(osigs) && cache.AddSnapshot(s) {
		topo := &common.SnapshotWithTopologicalOrder{
			Snapshot:         *s,
			TopologicalOrder: node.TopoCounter.Next(),
		}
		err := node.store.WriteSnapshot(topo)
		if err != nil {
			return true, err
		}
		s.Signatures = append([]crypto.Signature{}, osigs...)
	}
	node.Graph.CacheRound[s.NodeId] = cache
	node.Graph.FinalRound[s.NodeId] = final
	return false, nil
}

func (node *Node) tryToSignSnapshot(s *common.Snapshot) error {
	if s.NodeId != node.IdForNetwork || len(s.Signatures) != 0 || s.Timestamp != 0 {
		return nil
	}
	cache := node.Graph.CacheRound[s.NodeId].Copy()
	final := node.Graph.FinalRound[s.NodeId].Copy()
	for {
		s.Timestamp = uint64(time.Now().UnixNano())
		if s.Timestamp > cache.Timestamp {
			break
		}
		time.Sleep(1 * time.Millisecond)
	}

	cacheStart, _ := cache.Gap()
	if s.Timestamp >= config.SnapshotRoundGap+cacheStart {
		final = cache.asFinal()
		best := &FinalRound{NodeId: final.NodeId}
		for _, r := range node.Graph.FinalRound {
			if r.NodeId == s.NodeId || r.Start < best.Start {
				continue
			}
			if r.Start+config.SnapshotRoundGap < uint64(time.Now().UnixNano()) {
				best = r
			}
		}
		if best.NodeId == final.NodeId {
			panic(node.IdForNetwork)
		}

		cache = &CacheRound{
			NodeId:     s.NodeId,
			Number:     final.Number + 1,
			References: [2]crypto.Hash{final.Hash, best.Hash},
		}
		err := node.store.StartNewRound(cache.NodeId, cache.Number, cache.References, final.Start)
		if err != nil {
			panic(err)
		}
	}
	cache.Timestamp = s.Timestamp

	s.RoundNumber = cache.Number
	s.References = cache.References
	node.signSnapshot(s)
	node.Graph.CacheRound[s.NodeId] = cache
	node.Graph.FinalRound[s.NodeId] = final
	return nil
}

func (node *Node) verifyReferences(s *common.Snapshot, cache *CacheRound) (*FinalRound, error) {
	if s.RoundNumber != cache.Number+1 {
		return nil, nil
	}
	final := cache.asFinal()
	if final == nil {
		return nil, nil
	}
	if s.References[0] != final.Hash {
		return nil, nil
	}

	external, err := node.store.ReadRound(s.References[1])
	if err != nil {
		return nil, err
	}
	if external == nil {
		return nil, nil
	}
	link, err := node.store.ReadLink(s.NodeId, external.NodeId)
	if external.Number >= link {
		return final, err
	}
	return nil, err
}

func (node *Node) verifyTransactionInSnapshot(s *common.Snapshot) (bool, error) {
	in, err := node.store.CheckTransactionInNode(s.NodeId, s.Transaction)
	if err != nil {
		return true, err
	} else if in {
		return false, fmt.Errorf("transaction %s already snapshot by node %s", s.Transaction.String(), s.NodeId.String())
	}

	finalized, err := node.store.CheckTransactionFinalization(s.Transaction)
	if err != nil {
		return true, err
	}
	snapFinalized := node.verifyFinalization(s.Signatures)
	if finalized && !snapFinalized {
		return false, fmt.Errorf("transaction %s already finalized, won't sign it any more", s.Transaction.String())
	}
	if finalized {
		return false, nil
	}

	tx, err := node.store.ReadTransaction(s.Transaction)
	if err != nil || tx != nil {
		return false, err
	}
	signed, err := node.store.CacheGetTransaction(s.Transaction)
	if err != nil {
		return false, err
	}
	if signed == nil {
		return false, fmt.Errorf("transaction %s expired in cache", s.Transaction.String())
	}
	if !snapFinalized {
		err = signed.Validate(node.store)
		if err != nil {
			return true, err
		}
	}
	err = signed.LockInputs(node.store, snapFinalized)
	if err != nil {
		return true, err
	}
	err = node.store.WriteTransaction(signed)
	if err != nil {
		return true, err
	}
	return false, nil
}

func (node *Node) signSnapshot(s *common.Snapshot) {
	s.Hash = s.PayloadHash()
	sig := node.Account.PrivateSpendKey.Sign(s.Hash[:])
	osigs := node.SnapshotsPool[s.Hash]
	for _, o := range osigs {
		if o == sig {
			panic("should never be here")
		}
	}
	node.SnapshotsPool[s.Hash] = append(osigs, sig)
	node.SignaturesPool[s.Hash] = sig
}

func (node *Node) verifyFinalization(sigs []crypto.Signature) bool {
	consensusThreshold := len(node.ConsensusNodes) * 2 / 3
	return len(sigs) > consensusThreshold
}
