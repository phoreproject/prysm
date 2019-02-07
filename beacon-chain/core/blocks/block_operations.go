// Package blocks contains block processing libraries. These libraries
// process and verify block specific messages such as PoW receipt root,
// RANDAO, validator deposits, exits and slashing proofs.
package blocks

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"

	"github.com/gogo/protobuf/proto"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/helpers"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/state/stateutils"
	v "github.com/prysmaticlabs/prysm/beacon-chain/core/validators"
	pb "github.com/prysmaticlabs/prysm/proto/beacon/p2p/v1"
	"github.com/prysmaticlabs/prysm/shared/bytesutil"
	"github.com/prysmaticlabs/prysm/shared/hashutil"
	"github.com/prysmaticlabs/prysm/shared/mathutil"
	"github.com/prysmaticlabs/prysm/shared/params"
	"github.com/prysmaticlabs/prysm/shared/trieutil"
)

// VerifyProposerSignature uses BLS signature verification to ensure
// the correct proposer created an incoming beacon block during state
// transition processing.
//
// WIP - this is stubbed out until BLS is integrated into Prysm.
func VerifyProposerSignature(
	block *pb.BeaconBlock,
) error {
	if block == nil {
		return errors.New("received nil block")
	}
	return nil
}

// ProcessEth1Data is an operation performed on each
// beacon block to ensure the ETH1 data votes are processed
// into the beacon state.
//
// Official spec definition of ProcessEth1Data
//   If block.eth1_data equals eth1_data_vote.eth1_data for some eth1_data_vote
//   in state.eth1_data_votes, set eth1_data_vote.vote_count += 1.
//   Otherwise, append to state.eth1_data_votes a new Eth1DataVote(eth1_data=block.eth1_data, vote_count=1).
func ProcessEth1Data(beaconState *pb.BeaconState, block *pb.BeaconBlock) *pb.BeaconState {
	var eth1DataVoteAdded bool

	for idx := range beaconState.Eth1DataVotes {
		if proto.Equal(beaconState.Eth1DataVotes[idx].Eth1Data, block.Eth1Data) {
			beaconState.Eth1DataVotes[idx].VoteCount++
			eth1DataVoteAdded = true
			break
		}
	}

	if !eth1DataVoteAdded {
		beaconState.Eth1DataVotes = append(
			beaconState.Eth1DataVotes,
			&pb.Eth1DataVote{
				Eth1Data:  block.Eth1Data,
				VoteCount: 1,
			},
		)
	}

	return beaconState
}

// ProcessBlockRandao checks the block proposer's
// randao commitment and generates a new randao mix to update
// in the beacon state's latest randao mixes and set the proposer's randao fields.
//
// Official spec definition for block randao verification:
//   Let repeat_hash(x, n) = x if n == 0 else repeat_hash(hash(x), n-1).
//   Let proposer = state.validator_registry[get_beacon_proposer_index(state, state.slot)].
//   Verify that repeat_hash(block.randao_reveal, proposer.randao_layers) == proposer.randao_commitment.
//   Set state.latest_randao_mixes[state.slot % LATEST_RANDAO_MIXES_LENGTH] =
//     xor(state.latest_randao_mixes[state.slot % LATEST_RANDAO_MIXES_LENGTH], block.randao_reveal)
//   Set proposer.randao_commitment = block.randao_reveal.
//   Set proposer.randao_layers = 0
func ProcessBlockRandao(beaconState *pb.BeaconState, block *pb.BeaconBlock) (*pb.BeaconState, error) {
	proposerIndex, err := v.BeaconProposerIdx(beaconState, beaconState.Slot)
	if err != nil {
		return nil, fmt.Errorf("could not fetch beacon proposer index: %v", err)
	}
	registry := beaconState.ValidatorRegistry
	proposer := registry[proposerIndex]
	if err := verifyBlockRandao(proposer, block); err != nil {
		return nil, fmt.Errorf("could not verify block randao: %v", err)
	}
	// If block randao passed verification, we XOR the state's latest randao mix with the block's
	// randao and update the state's corresponding latest randao mix value.
	latestMixesLength := params.BeaconConfig().LatestRandaoMixesLength
	latestMixSlice := beaconState.LatestRandaoMixesHash32S[beaconState.Slot%latestMixesLength]
	latestMix := bytesutil.ToBytes32(latestMixSlice)
	for i, x := range block.RandaoRevealHash32 {
		latestMix[i] ^= x
	}
	proposer.RandaoCommitmentHash32 = block.RandaoRevealHash32
	proposer.RandaoLayers = 0
	registry[proposerIndex] = proposer
	beaconState.LatestRandaoMixesHash32S[beaconState.Slot%latestMixesLength] = latestMix[:]
	beaconState.ValidatorRegistry = registry
	return beaconState, nil
}

func verifyBlockRandao(proposer *pb.Validator, block *pb.BeaconBlock) error {
	blockRandaoReveal := bytesutil.ToBytes32(block.RandaoRevealHash32)
	proposerRandaoCommit := bytesutil.ToBytes32(proposer.RandaoCommitmentHash32)
	randaoHashLayers := hashutil.RepeatHash(blockRandaoReveal, proposer.RandaoLayers)
	// Verify that repeat_hash(block.randao_reveal, proposer.randao_layers) == proposer.randao_commitment.
	if randaoHashLayers != proposerRandaoCommit {
		return fmt.Errorf(
			"expected hashed block randao layers to equal proposer randao: received %#x = %#x",
			randaoHashLayers[:],
			proposerRandaoCommit[:],
		)
	}
	return nil
}

// ProcessProposerSlashings is one of the operations performed
// on each processed beacon block to penalize proposers based on
// slashing conditions if any slashable events occurred.
//
// Official spec definition for proposer slashings:
//   Verify that len(block.body.proposer_slashings) <= MAX_PROPOSER_SLASHINGS.
//
//   For each proposer_slashing in block.body.proposer_slashings:
//     Let proposer = state.validator_registry[proposer_slashing.proposer_index].
//     Verify that proposer_slashing.proposal_data_1.slot == proposer_slashing.proposal_data_2.slot.
//     Verify that proposer_slashing.proposal_data_1.shard == proposer_slashing.proposal_data_2.shard.
//     Verify that proposer_slashing.proposal_data_1.block_root != proposer_slashing.proposal_data_2.block_root.
//     Verify that proposer.penalized_epoch > get_current_epoch(state).
//     Verify that bls_verify(pubkey=proposer.pubkey, message=hash_tree_root(proposer_slashing.proposal_data_1),
//       signature=proposer_slashing.proposal_signature_1,
//       domain=get_domain(state.fork, slot_to_epoch(proposer_slashing.proposal_data_1.slot), DOMAIN_PROPOSAL)).
//     Verify that bls_verify(pubkey=proposer.pubkey, message=hash_tree_root(proposer_slashing.proposal_data_2),
//       signature=proposer_slashing.proposal_signature_2,
//       domain=get_domain(state.fork, slot_to_epoch(proposer_slashing.proposal_data_2.slot), DOMAIN_PROPOSAL)).
//     Run penalize_validator(state, proposer_slashing.proposer_index).
func ProcessProposerSlashings(
	beaconState *pb.BeaconState,
	block *pb.BeaconBlock,
	verifySignatures bool,
) (*pb.BeaconState, error) {
	body := block.Body
	registry := beaconState.ValidatorRegistry
	if uint64(len(body.ProposerSlashings)) > params.BeaconConfig().MaxProposerSlashings {
		return nil, fmt.Errorf(
			"number of proposer slashings (%d) exceeds allowed threshold of %d",
			len(body.ProposerSlashings),
			params.BeaconConfig().MaxProposerSlashings,
		)
	}
	var err error
	for idx, slashing := range body.ProposerSlashings {
		if err = verifyProposerSlashing(slashing, verifySignatures); err != nil {
			return nil, fmt.Errorf("could not verify proposer slashing #%d: %v", idx, err)
		}
		proposer := registry[slashing.ProposerIndex]
		if proposer.PenalizedEpoch > helpers.CurrentEpoch(beaconState) {
			beaconState, err = v.PenalizeValidator(beaconState, slashing.ProposerIndex)
			if err != nil {
				return nil, fmt.Errorf("could not penalize proposer index %d: %v",
					slashing.ProposerIndex, err)
			}
		}
	}
	return beaconState, nil
}

func verifyProposerSlashing(
	slashing *pb.ProposerSlashing,
	verifySignatures bool,
) error {
	// section of block operations.
	slot1 := slashing.ProposalData_1.Slot
	slot2 := slashing.ProposalData_2.Slot
	shard1 := slashing.ProposalData_1.Shard
	shard2 := slashing.ProposalData_2.Shard
	root1 := slashing.ProposalData_1.BlockRootHash32
	root2 := slashing.ProposalData_2.BlockRootHash32
	if slot1 != slot2 {
		return fmt.Errorf("slashing proposal data slots do not match: %d, %d", slot1, slot2)
	}
	if shard1 != shard2 {
		return fmt.Errorf("slashing proposal data shards do not match: %d, %d", shard1, shard2)
	}
	if !bytes.Equal(root1, root2) {
		return fmt.Errorf("slashing proposal data block roots do not match: %#x, %#x", root1, root2)
	}
	if verifySignatures {
		// TODO(#258): Verify BLS according to the specification in the "Proposer Slashings"
		return nil
	}
	return nil
}

// ProcessAttesterSlashings is one of the operations performed
// on each processed beacon block to penalize attesters based on
// Casper FFG slashing conditions if any slashable events occurred.
//
// Official spec definition for attester slashings:
//
//   Verify that len(block.body.attester_slashings) <= MAX_ATTESTER_SLASHINGS.
//
//   For each attester_slashing in block.body.attester_slashings:
//     Let slashable_attestation_1 = attester_slashing.slashable_attestation_1.
//     Let slashable_attestation_2 = attester_slashing.slashable_attestation_2.
//     Verify that slashable_attestation_1.data != slashable_attestation_2.data.
//     Verify that is_double_vote(slashable_attestation_1.data, slashable_attestation_2.data)
//       or is_surround_vote(slashable_attestation_1.data, slashable_attestation_2.data).
//     Verify that verify_slashable_attestation(state, slashable_attestation_1).
//     Verify that verify_slashable_attestation(state, slashable_attestation_2).
//     Let slashable_indices = [index for index in slashable_attestation_1.validator_indices if
//       index in slashable_attestation_2.validator_indices and
//       state.validator_registry[index].penalized_epoch > get_current_epoch(state)].
//     Verify that len(slashable_indices) >= 1.
//     Run penalize_validator(state, index) for each index in slashable_indices.
func ProcessAttesterSlashings(
	beaconState *pb.BeaconState,
	block *pb.BeaconBlock,
	verifySignatures bool,
) (*pb.BeaconState, error) {
	body := block.Body
	if uint64(len(body.AttesterSlashings)) > params.BeaconConfig().MaxAttesterSlashings {
		return nil, fmt.Errorf(
			"number of attester slashings (%d) exceeds allowed threshold of %d",
			len(body.AttesterSlashings),
			params.BeaconConfig().MaxAttesterSlashings,
		)
	}
	for idx, slashing := range body.AttesterSlashings {
		if err := verifyAttesterSlashing(slashing, verifySignatures); err != nil {
			return nil, fmt.Errorf("could not verify attester slashing #%d: %v", idx, err)
		}
		slashableIndices, err := attesterSlashableIndices(beaconState, slashing)
		if err != nil {
			return nil, fmt.Errorf("could not determine validator indices to slash: %v", err)
		}
		for _, validatorIndex := range slashableIndices {
			beaconState, err = v.PenalizeValidator(beaconState, validatorIndex)
			if err != nil {
				return nil, fmt.Errorf("could not penalize validator index %d: %v",
					validatorIndex, err)
			}
		}
	}
	return beaconState, nil
}

func verifyAttesterSlashing(slashing *pb.AttesterSlashing, verifySignatures bool) error {
	slashableAttestation1 := slashing.SlashableAttestation_1
	slashableAttestation2 := slashing.SlashableAttestation_2
	data1 := slashableAttestation1.Data
	data2 := slashableAttestation2.Data
	// Inner attestation data structures for the votes should not be equal,
	// as that would mean both votes are the same and therefore no slashing
	// should occur.
	if reflect.DeepEqual(data1, data2) {
		return fmt.Errorf(
			"attester slashing inner slashable vote data attestation should not match: %v, %v",
			data1,
			data2,
		)
	}
	// Verify the slashing is a double vote or a surround vote.
	if !(isDoubleVote(data1, data2) || isSurroundVote(data1, data2)) {
		return errors.New("attester slashing is not a double vote nor surround vote")
	}
	if err := verifySlashableAttestation(slashableAttestation1, verifySignatures); err != nil {
		return fmt.Errorf("could not verify attester slashable attestation data 1: %v", err)
	}
	if err := verifySlashableAttestation(slashableAttestation2, verifySignatures); err != nil {
		return fmt.Errorf("could not verify attester slashable attestation data 2: %v", err)
	}
	return nil
}

func attesterSlashableIndices(beaconState *pb.BeaconState, slashing *pb.AttesterSlashing) ([]uint64, error) {
	slashableAttestation1 := slashing.SlashableAttestation_1
	slashableAttestation2 := slashing.SlashableAttestation_2
	// Let slashable_indices = [index for index in slashable_attestation_1.validator_indices if
	//   index in slashable_attestation_2.validator_indices and
	//   state.validator_registry[index].penalized_epoch > get_current_epoch(state)].
	var slashableIndices []uint64
	for _, idx1 := range slashableAttestation1.ValidatorIndices {
		for _, idx2 := range slashableAttestation2.ValidatorIndices {
			if idx1 == idx2 {
				if beaconState.ValidatorRegistry[idx1].PenalizedEpoch > helpers.CurrentEpoch(beaconState) {
					slashableIndices = append(slashableIndices, idx1)
				}
			}
		}
	}
	// Verify that len(slashable_indices) >= 1.
	if len(slashableIndices) < 1 {
		return nil, errors.New("expected a non-empty list of slashable indices")
	}
	return slashableIndices, nil
}

func verifySlashableAttestation(att *pb.SlashableAttestation, verifySignatures bool) error {
	emptyCustody := make([]byte, len(att.CustodyBitfield))
	if bytes.Equal(att.CustodyBitfield, emptyCustody) {
		return errors.New("custody bit field can't all be 0s")
	}
	if len(att.ValidatorIndices) == 0 {
		return errors.New("empty validator indices")
	}
	for i := 0; i < len(att.ValidatorIndices)-1; i++ {
		if att.ValidatorIndices[i] >= att.ValidatorIndices[i+1] {
			return fmt.Errorf("validator indices not in descending order: %v",
				att.ValidatorIndices)
		}
	}
	if len(att.CustodyBitfield) != mathutil.CeilDiv8(len(att.ValidatorIndices)) {
		return fmt.Errorf("custody bit field length (%d) don't match indices length (%d)",
			len(att.CustodyBitfield), mathutil.CeilDiv8(len(att.ValidatorIndices)))
	}
	if uint64(len(att.ValidatorIndices)) > params.BeaconConfig().MaxIndicesPerSlashableVote {
		return fmt.Errorf("validator indices length (%d) exceeded max indices per slashable vote(%d)",
			len(att.ValidatorIndices), params.BeaconConfig().MaxIndicesPerSlashableVote)
	}

	if verifySignatures {
		// TODO(#258): Implement BLS verify multiple.
		return nil
	}
	return nil
}

func isDoubleVote(data1 *pb.AttestationData, data2 *pb.AttestationData) bool {
	return helpers.SlotToEpoch(data1.Slot) == helpers.SlotToEpoch(data2.Slot)
}

func isSurroundVote(data1 *pb.AttestationData, data2 *pb.AttestationData) bool {
	sourceEpoch1 := data1.JustifiedEpoch
	sourceEpoch2 := data2.JustifiedEpoch
	targetEpoch1 := helpers.SlotToEpoch(data1.Slot)
	targetEpoch2 := helpers.SlotToEpoch(data2.Slot)
	return sourceEpoch1 < sourceEpoch2 && targetEpoch2 < targetEpoch1
}

// ProcessBlockAttestations applies processing operations to a block's inner attestation
// records. This function returns a list of pending attestations which can then be
// appended to the BeaconState's latest attestations.
//
// Official spec definition for block attestation processing:
//   Verify that len(block.body.attestations) <= MAX_ATTESTATIONS.
//
//   For each attestation in block.body.attestations:
//     Verify that attestation.data.slot <= state.slot - MIN_ATTESTATION_INCLUSION_DELAY <
//       attestation.data.slot + EPOCH_LENGTH.
//     Verify that attestation.data.justified_epoch is equal to state.justified_epoch
//       if attestation.data.slot >= get_epoch_start_slot(get_current_epoch(state)) else state.previous_justified_epoch.
//     Verify that attestation.data.justified_block_root is equal to
//       get_block_root(state, get_epoch_start_slot(attestation.data.justified_epoch)).
//     Verify that either attestation.data.latest_crosslink_root or
//       attestation.data.shard_block_root equals state.latest_crosslinks[shard].shard_block_root.
//     Verify bitfields and aggregate signature using BLS.
//     [TO BE REMOVED IN PHASE 1] Verify that attestation.data.shard_block_root == ZERO_HASH.
//     Append PendingAttestation(data=attestation.data, aggregation_bitfield=attestation.aggregation_bitfield,
//       custody_bitfield=attestation.custody_bitfield, inclusion_slot=state.slot) to state.latest_attestations
func ProcessBlockAttestations(
	beaconState *pb.BeaconState,
	block *pb.BeaconBlock,
	verifySignatures bool,
) (*pb.BeaconState, error) {
	atts := block.Body.Attestations
	if uint64(len(atts)) > params.BeaconConfig().MaxAttestations {
		return nil, fmt.Errorf(
			"number of attestations in block (%d) exceeds allowed threshold of %d",
			len(atts),
			params.BeaconConfig().MaxAttestations,
		)
	}
	var pendingAttestations []*pb.PendingAttestationRecord
	for idx, attestation := range atts {
		if err := verifyAttestation(beaconState, attestation, verifySignatures); err != nil {
			return nil, fmt.Errorf("could not verify attestation at index %d in block: %v", idx, err)
		}
		pendingAttestations = append(pendingAttestations, &pb.PendingAttestationRecord{
			Data:                attestation.Data,
			AggregationBitfield: attestation.AggregationBitfield,
			CustodyBitfield:     attestation.CustodyBitfield,
			SlotIncluded:        beaconState.Slot,
		})
	}
	beaconState.LatestAttestations = pendingAttestations
	return beaconState, nil
}

func verifyAttestation(beaconState *pb.BeaconState, att *pb.Attestation, verifySignatures bool) error {
	inclusionDelay := params.BeaconConfig().MinAttestationInclusionDelay
	if att.Data.Slot+inclusionDelay > beaconState.Slot {
		return fmt.Errorf(
			"attestation slot (slot %d) + inclusion delay (%d) beyond current beacon state slot (%d)",
			att.Data.Slot,
			inclusionDelay,
			beaconState.Slot,
		)
	}
	if att.Data.Slot+params.BeaconConfig().EpochLength < beaconState.Slot {
		return fmt.Errorf(
			"attestation slot (slot %d) + epoch length (%d) less than current beacon state slot (%d)",
			att.Data.Slot,
			params.BeaconConfig().EpochLength,
			beaconState.Slot,
		)
	}
	// Verify that attestation.data.justified_epoch is equal to state.justified_epoch
	// 	if attestation.data.slot >= get_epoch_start_slot(get_current_epoch(state))
	// 	else state.previous_justified_epoch.
	if att.Data.Slot >= helpers.StartSlot(helpers.SlotToEpoch(beaconState.Slot)) {
		if att.Data.JustifiedEpoch != beaconState.JustifiedEpoch {
			return fmt.Errorf(
				"expected attestation.JustifiedEpoch == state.JustifiedEpoch, received %d == %d",
				att.Data.JustifiedEpoch,
				beaconState.JustifiedEpoch,
			)
		}
	} else {
		if att.Data.JustifiedEpoch != beaconState.PreviousJustifiedEpoch {
			return fmt.Errorf(
				"expected attestation.JustifiedEpoch == state.PreviousJustifiedEpoch, received %d == %d",
				att.Data.JustifiedEpoch,
				beaconState.PreviousJustifiedEpoch,
			)
		}
	}

	// Verify that attestation.data.justified_block_root is equal to
	// get_block_root(state, get_epoch_start_slot(attestation.data.justified_epoch)).
	blockRoot, err := BlockRoot(beaconState, helpers.StartSlot(att.Data.JustifiedEpoch))
	if err != nil {
		return fmt.Errorf("could not get block root for justified slot: %v", err)
	}

	justifiedBlockRoot := att.Data.JustifiedBlockRootHash32
	if !bytes.Equal(justifiedBlockRoot, blockRoot) {
		return fmt.Errorf(
			"expected JustifiedBlockRoot == getBlockRoot(state, JustifiedSlot): got %#x = %#x",
			justifiedBlockRoot,
			blockRoot,
		)
	}

	// Verify that either: attestation.data.latest_crosslink_root or
	// attestation.data.shard_block_root equals
	// state.latest_crosslinks[shard].shard_block_root
	crossLinkRoot := att.Data.LatestCrosslinkRootHash32
	shardBlockRoot := att.Data.ShardBlockRootHash32
	shard := att.Data.Shard
	stateShardBlockRoot := beaconState.LatestCrosslinks[shard].ShardBlockRootHash32

	if !(bytes.Equal(crossLinkRoot, stateShardBlockRoot) ||
		bytes.Equal(shardBlockRoot, stateShardBlockRoot)) {
		return fmt.Errorf(
			"attestation.CrossLinkRoot and ShardBlockRoot != %v (state.LatestCrosslinks' ShardBlockRoot)",
			stateShardBlockRoot,
		)
	}

	// Verify attestation.shard_block_root == ZERO_HASH [TO BE REMOVED IN PHASE 1].
	if !bytes.Equal(att.Data.ShardBlockRootHash32, []byte{}) {
		return fmt.Errorf(
			"expected attestation.ShardBlockRoot == %#x, received %#x instead",
			[]byte{},
			att.Data.ShardBlockRootHash32,
		)
	}
	if verifySignatures {
		// TODO(#258): Integrate BLS signature verification for attestation.
		// assert bls_verify_multiple(
		//   pubkeys=[
		//	 bls_aggregate_pubkeys([state.validator_registry[i].pubkey for i in custody_bit_0_participants]),
		//   bls_aggregate_pubkeys([state.validator_registry[i].pubkey for i in custody_bit_1_participants]),
		//   ],
		//   messages=[
		//   hash_tree_root(AttestationDataAndCustodyBit(data=attestation.data, custody_bit=0b0)),
		//   hash_tree_root(AttestationDataAndCustodyBit(data=attestation.data, custody_bit=0b1)),
		//   ],
		//   signature=attestation.aggregate_signature,
		//   domain=get_domain(state.fork, slot_to_epoch(attestation.data.slot), DOMAIN_ATTESTATION),
		// )
		return nil
	}
	return nil
}

// ProcessValidatorDeposits is one of the operations performed on each processed
// beacon block to verify queued validators from the Ethereum 1.0 Deposit Contract
// into the beacon chain.
//
// Official spec definition for processing validator deposits:
//   Verify that len(block.body.deposits) <= MAX_DEPOSITS.
//   For each deposit in block.body.deposits:
//     Let serialized_deposit_data be the serialized form of deposit.deposit_data.
//     It should be the DepositInput followed by 8 bytes for deposit_data.value
//     and 8 bytes for deposit_data.timestamp. That is, it should match
//     deposit_data in the Ethereum 1.0 deposit contract of which the hash
//     was placed into the Merkle tree.
//
//     Verify deposit merkle_branch, setting leaf=hash(serialized_deposit_data), branch=deposit.branch,
//     depth=DEPOSIT_CONTRACT_TREE_DEPTH and root=state.latest_eth1_data.deposit_root, index = deposit.index:
//
//     Run the following:
//     process_deposit(
//       state=state,
//       pubkey=deposit.deposit_data.deposit_input.pubkey,
//       deposit=deposit.deposit_data.value,
//       proof_of_possession=deposit.deposit_data.deposit_input.proof_of_possession,
//       withdrawal_credentials=deposit.deposit_data.deposit_input.withdrawal_credentials,
//     )
func ProcessValidatorDeposits(
	beaconState *pb.BeaconState,
	block *pb.BeaconBlock,
) (*pb.BeaconState, error) {
	deposits := block.Body.Deposits
	if uint64(len(deposits)) > params.BeaconConfig().MaxDeposits {
		return nil, fmt.Errorf(
			"number of deposits (%d) exceeds allowed threshold of %d",
			len(deposits),
			params.BeaconConfig().MaxDeposits,
		)
	}
	var err error
	var depositInput *pb.DepositInput
	validatorIndexMap := stateutils.ValidatorIndexMap(beaconState)
	for idx, deposit := range deposits {
		depositData := deposit.DepositData
		depositInput, err = DecodeDepositInput(depositData)
		if err != nil {
			return nil, fmt.Errorf("could not decode deposit input: %v", err)
		}
		if err = verifyDeposit(beaconState, deposit); err != nil {
			return nil, fmt.Errorf("could not verify deposit #%d: %v", idx, err)
		}
		// depositData consists of depositValue [8]byte +
		// depositTimestamp [8]byte + depositInput []byte .
		depositValue := depositData[:8]
		// We then mutate the beacon state with the verified validator deposit.
		beaconState, err = v.ProcessDeposit(
			beaconState,
			validatorIndexMap,
			depositInput.Pubkey,
			binary.BigEndian.Uint64(depositValue),
			depositInput.ProofOfPossession,
			depositInput.WithdrawalCredentialsHash32,
		)
		if err != nil {
			return nil, fmt.Errorf("could not process deposit into beacon state: %v", err)
		}
	}
	return beaconState, nil
}

func verifyDeposit(beaconState *pb.BeaconState, deposit *pb.Deposit) error {
	// Verify Merkle proof of deposit and deposit trie root.
	receiptRoot := bytesutil.ToBytes32(beaconState.LatestEth1Data.DepositRootHash32)
	if ok := trieutil.VerifyMerkleBranch(
		hashutil.Hash(deposit.DepositData),
		deposit.MerkleBranchHash32S,
		params.BeaconConfig().DepositContractTreeDepth,
		deposit.MerkleTreeIndex,
		receiptRoot,
	); !ok {
		return fmt.Errorf(
			"deposit merkle branch of deposit root did not verify for root: %#x",
			receiptRoot,
		)
	}

	return nil
}

// ProcessValidatorExits is one of the operations performed
// on each processed beacon block to determine which validators
// should exit the state's validator registry.
//
// Official spec definition for processing exits:
//
//   Verify that len(block.body.exits) <= MAX_EXITS.
//
//   For each exit in block.body.exits:
//     Let validator = state.validator_registry[exit.validator_index].
//     Verify that validator.exit_epoch > get_entry_exit_effect_epoch(get_current_epoch(state)).
//     Verify that get_current_epoch(state) >= exit.epoch.
//     Let exit_message = hash_tree_root(
//       Exit(epoch=exit.epoch, validator_index=exit.validator_index, signature=EMPTY_SIGNATURE)
//     )
//     Verify that bls_verify(pubkey=validator.pubkey, message=exit_message,
//       signature=exit.signature, domain=get_domain(state.fork, exit.epoch, DOMAIN_EXIT)).
//     Run initiate_validator_exit(state, exit.validator_index).
func ProcessValidatorExits(
	beaconState *pb.BeaconState,
	block *pb.BeaconBlock,
	verifySignatures bool,
) (*pb.BeaconState, error) {
	exits := block.Body.Exits
	if uint64(len(exits)) > params.BeaconConfig().MaxExits {
		return nil, fmt.Errorf(
			"number of exits (%d) exceeds allowed threshold of %d",
			len(exits),
			params.BeaconConfig().MaxExits,
		)
	}

	validatorRegistry := beaconState.ValidatorRegistry
	for idx, exit := range exits {
		if err := verifyExit(beaconState, exit, verifySignatures); err != nil {
			return nil, fmt.Errorf("could not verify exit #%d: %v", idx, err)
		}
		beaconState = v.InitiateValidatorExit(beaconState, exit.ValidatorIndex)
	}
	beaconState.ValidatorRegistry = validatorRegistry
	return beaconState, nil
}

func verifyExit(beaconState *pb.BeaconState, exit *pb.Exit, verifySignatures bool) error {
	validator := beaconState.ValidatorRegistry[exit.ValidatorIndex]
	currentEpoch := helpers.CurrentEpoch(beaconState)
	entryExitEffectEpoch := helpers.EntryExitEffectEpoch(currentEpoch)
	if validator.ExitEpoch <= entryExitEffectEpoch {
		return fmt.Errorf(
			"validator exit epoch should be > entry_exit_effect_epoch, received %d <= %d",
			currentEpoch,
			entryExitEffectEpoch,
		)
	}
	if currentEpoch < exit.Epoch {
		return fmt.Errorf(
			"expected current epoch >= exit.epoch, received %d < %d",
			currentEpoch,
			exit.Epoch,
		)
	}
	if verifySignatures {
		// TODO(#258): Verify using BLS signature verification below:
		// Let exit_message = hash_tree_root(
		//   Exit(epoch=exit.epoch, validator_index=exit.validator_index, signature=EMPTY_SIGNATURE)
		// )
		// Verify that bls_verify(pubkey=validator.pubkey, message=exit_message,
		//   signature=exit.signature, domain=get_domain(state.fork, exit.epoch, DOMAIN_EXIT)).
		return nil
	}
	return nil
}
