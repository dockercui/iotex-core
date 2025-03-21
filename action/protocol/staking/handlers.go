// Copyright (c) 2022 IoTeX Foundation
// This source code is provided 'as is' and no warranties are given as to title or non-infringement, merchantability
// or fitness for purpose and, to the extent permitted by law, all liability for your use of the code is disclaimed.
// This source code is governed by Apache License 2.0 that can be found in the LICENSE file.

package staking

import (
	"context"
	"math/big"
	"time"

	"github.com/iotexproject/go-pkgs/hash"
	"github.com/iotexproject/iotex-address/address"
	"github.com/iotexproject/iotex-proto/golang/iotextypes"
	"github.com/pkg/errors"

	"github.com/iotexproject/iotex-core/v2/action"
	"github.com/iotexproject/iotex-core/v2/action/protocol"
	accountutil "github.com/iotexproject/iotex-core/v2/action/protocol/account/util"
	"github.com/iotexproject/iotex-core/v2/pkg/util/byteutil"
	"github.com/iotexproject/iotex-core/v2/state"
)

// constants
const (
	HandleCreateStake       = "createStake"
	HandleUnstake           = "unstake"
	HandleWithdrawStake     = "withdrawStake"
	HandleChangeCandidate   = "changeCandidate"
	HandleTransferStake     = "transferStake"
	HandleDepositToStake    = "depositToStake"
	HandleRestake           = "restake"
	HandleCandidateRegister = "candidateRegister"
	HandleCandidateUpdate   = "candidateUpdate"
)

const _withdrawWaitingTime = 14 * 24 * time.Hour // to maintain backward compatibility with r0.11 code

// Errors and vars
var (
	ErrNilParameters = errors.New("parameter is nil")
	errCandNotExist  = &handleError{
		err:           ErrInvalidOwner,
		failureStatus: iotextypes.ReceiptStatus_ErrCandidateNotExist,
	}
)

type handleError struct {
	err           error
	failureStatus iotextypes.ReceiptStatus
}

func (h *handleError) Error() string {
	return h.err.Error()
}

func (h *handleError) ReceiptStatus() uint64 {
	return uint64(h.failureStatus)
}

func (p *Protocol) handleCreateStake(ctx context.Context, act *action.CreateStake, csm CandidateStateManager,
) (*receiptLog, []*action.TransactionLog, error) {
	actionCtx := protocol.MustGetActionCtx(ctx)
	blkCtx := protocol.MustGetBlockCtx(ctx)
	featureCtx := protocol.MustGetFeatureCtx(ctx)
	log := newReceiptLog(p.addr.String(), HandleCreateStake, featureCtx.NewStakingReceiptFormat)

	staker, fetchErr := fetchCaller(ctx, csm, act.Amount())
	if fetchErr != nil {
		return log, nil, fetchErr
	}

	// Create new bucket and bucket index
	candidate := csm.GetByName(act.Candidate())
	if candidate == nil {
		return log, nil, errCandNotExist
	}
	bucket := NewVoteBucket(candidate.GetIdentifier(), actionCtx.Caller, act.Amount(), act.Duration(), blkCtx.BlockTimeStamp, act.AutoStake())
	bucketIdx, err := csm.putBucketAndIndex(bucket)
	if err != nil {
		return log, nil, err
	}
	log.AddTopics(byteutil.Uint64ToBytesBigEndian(bucketIdx), candidate.GetIdentifier().Bytes())

	// update candidate
	weightedVote := p.calculateVoteWeight(bucket, false)
	if err := candidate.AddVote(weightedVote); err != nil {
		return log, nil, &handleError{
			err:           errors.Wrapf(err, "failed to add vote for candidate %s", candidate.GetIdentifier().String()),
			failureStatus: iotextypes.ReceiptStatus_ErrInvalidBucketAmount,
		}
	}
	if err := csm.Upsert(candidate); err != nil {
		return log, nil, csmErrorToHandleError(candidate.GetIdentifier().String(), err)
	}

	// update bucket pool
	if err := csm.DebitBucketPool(act.Amount(), true); err != nil {
		return log, nil, &handleError{
			err:           errors.Wrapf(err, "failed to update staking bucket pool %s", err.Error()),
			failureStatus: iotextypes.ReceiptStatus_ErrWriteAccount,
		}
	}

	// update staker balance
	if err := staker.SubBalance(act.Amount()); err != nil {
		return log, nil, &handleError{
			err:           errors.Wrapf(err, "failed to update the balance of staker %s", actionCtx.Caller.String()),
			failureStatus: iotextypes.ReceiptStatus_ErrNotEnoughBalance,
		}
	}
	// put updated staker's account state to trie
	if err := accountutil.StoreAccount(csm.SM(), actionCtx.Caller, staker); err != nil {
		return log, nil, errors.Wrapf(err, "failed to store account %s", actionCtx.Caller.String())
	}

	log.AddAddress(candidate.GetIdentifier())
	log.AddAddress(actionCtx.Caller)
	log.SetData(byteutil.Uint64ToBytesBigEndian(bucketIdx))

	return log, []*action.TransactionLog{
		{
			Type:      iotextypes.TransactionLogType_CREATE_BUCKET,
			Sender:    actionCtx.Caller.String(),
			Recipient: address.StakingBucketPoolAddr,
			Amount:    act.Amount(),
		},
	}, nil
}

func (p *Protocol) handleUnstake(ctx context.Context, act *action.Unstake, csm CandidateStateManager,
) (*receiptLog, error) {
	actionCtx := protocol.MustGetActionCtx(ctx)
	blkCtx := protocol.MustGetBlockCtx(ctx)
	featureCtx := protocol.MustGetFeatureCtx(ctx)
	log := newReceiptLog(p.addr.String(), HandleUnstake, featureCtx.NewStakingReceiptFormat)

	_, fetchErr := fetchCaller(ctx, csm, big.NewInt(0))
	if fetchErr != nil {
		return log, fetchErr
	}

	bucket, fetchErr := p.fetchBucketAndValidate(featureCtx, csm, actionCtx.Caller, act.BucketIndex(), true, true)
	if fetchErr != nil {
		return log, fetchErr
	}
	log.AddTopics(byteutil.Uint64ToBytesBigEndian(bucket.Index), bucket.Candidate.Bytes())

	candidate := csm.GetByIdentifier(bucket.Candidate)
	if candidate == nil {
		return log, errCandNotExist
	}

	if featureCtx.CannotUnstakeAgain && bucket.isUnstaked() {
		return log, &handleError{
			err:           errors.New("unstake an already unstaked bucket again not allowed"),
			failureStatus: iotextypes.ReceiptStatus_ErrInvalidBucketType,
		}
	}

	if bucket.AutoStake {
		return log, &handleError{
			err:           errors.New("AutoStake should be disabled first in order to unstake"),
			failureStatus: iotextypes.ReceiptStatus_ErrInvalidBucketType,
		}
	}

	if blkCtx.BlockTimeStamp.Before(bucket.StakeStartTime.Add(bucket.StakedDuration)) {
		return log, &handleError{
			err:           errors.New("bucket is not ready to be unstaked"),
			failureStatus: iotextypes.ReceiptStatus_ErrUnstakeBeforeMaturity,
		}
	}
	if !featureCtx.DisableDelegateEndorsement {
		if rErr := validateBucketWithoutEndorsement(ctx, NewEndorsementStateManager(csm.SM()), bucket, blkCtx.BlockHeight); rErr != nil {
			return log, rErr
		}
	}
	// TODO: cannot unstake if selected as candidates in this or next epoch

	if featureCtx.UnstakedButNotClearSelfStakeAmount {
		// update bucket
		bucket.UnstakeStartTime = blkCtx.BlockTimeStamp.UTC()
		if err := csm.updateBucket(act.BucketIndex(), bucket); err != nil {
			return log, errors.Wrapf(err, "failed to update bucket for voter %s", bucket.Owner.String())
		}
	}
	selfStake, err := isSelfStakeBucket(featureCtx, csm, bucket)
	if err != nil {
		return log, &handleError{
			err:           err,
			failureStatus: iotextypes.ReceiptStatus_ErrUnknown,
		}
	}
	if !featureCtx.UnstakedButNotClearSelfStakeAmount {
		// update bucket
		bucket.UnstakeStartTime = blkCtx.BlockTimeStamp.UTC()
		if err := csm.updateBucket(act.BucketIndex(), bucket); err != nil {
			return log, errors.Wrapf(err, "failed to update bucket for voter %s", bucket.Owner.String())
		}
	}
	weightedVote := p.calculateVoteWeight(bucket, selfStake)
	if err := candidate.SubVote(weightedVote); err != nil {
		return log, &handleError{
			err:           errors.Wrapf(err, "failed to subtract vote for candidate %s", bucket.Candidate.String()),
			failureStatus: iotextypes.ReceiptStatus_ErrNotEnoughBalance,
		}
	}
	// clear candidate's self stake if the bucket is self staking
	if selfStake {
		candidate.SelfStake = big.NewInt(0)
		if !featureCtx.UnstakedButNotClearSelfStakeAmount {
			candidate.SelfStakeBucketIdx = candidateNoSelfStakeBucketIndex
		}
	}
	if err := csm.Upsert(candidate); err != nil {
		return log, csmErrorToHandleError(candidate.GetIdentifier().String(), err)
	}

	log.AddAddress(actionCtx.Caller)
	return log, nil
}

func (p *Protocol) handleWithdrawStake(ctx context.Context, act *action.WithdrawStake, csm CandidateStateManager,
) (*receiptLog, []*action.TransactionLog, error) {
	actionCtx := protocol.MustGetActionCtx(ctx)
	blkCtx := protocol.MustGetBlockCtx(ctx)
	featureCtx := protocol.MustGetFeatureCtx(ctx)
	log := newReceiptLog(p.addr.String(), HandleWithdrawStake, featureCtx.NewStakingReceiptFormat)

	withdrawer, fetchErr := fetchCaller(ctx, csm, big.NewInt(0))
	if fetchErr != nil {
		return log, nil, fetchErr
	}

	bucket, fetchErr := p.fetchBucketAndValidate(featureCtx, csm, actionCtx.Caller, act.BucketIndex(), true, true)
	if fetchErr != nil {
		return log, nil, fetchErr
	}
	log.AddTopics(byteutil.Uint64ToBytesBigEndian(bucket.Index), bucket.Candidate.Bytes())

	// check unstake time
	cannotWithdraw := bucket.UnstakeStartTime.Unix() == 0
	if featureCtx.CannotUnstakeAgain {
		cannotWithdraw = !bucket.isUnstaked()
	}
	if cannotWithdraw {
		return log, nil, &handleError{
			err:           errors.New("bucket has not been unstaked"),
			failureStatus: iotextypes.ReceiptStatus_ErrWithdrawBeforeUnstake,
		}
	}

	withdrawWaitTime := p.config.WithdrawWaitingPeriod
	if !featureCtx.NewStakingReceiptFormat {
		withdrawWaitTime = _withdrawWaitingTime
	}
	if blkCtx.BlockTimeStamp.Before(bucket.UnstakeStartTime.Add(withdrawWaitTime)) {
		return log, nil, &handleError{
			err:           errors.New("stake is not ready to withdraw"),
			failureStatus: iotextypes.ReceiptStatus_ErrWithdrawBeforeMaturity,
		}
	}

	// delete bucket and bucket index
	if err := csm.delBucketAndIndex(bucket.Owner, bucket.Candidate, act.BucketIndex()); err != nil {
		return log, nil, errors.Wrapf(err, "failed to delete bucket for candidate %s", bucket.Candidate.String())
	}

	// update bucket pool
	if err := csm.CreditBucketPool(bucket.StakedAmount); err != nil {
		return log, nil, &handleError{
			err:           errors.Wrapf(err, "failed to update staking bucket pool %s", err.Error()),
			failureStatus: iotextypes.ReceiptStatus_ErrWriteAccount,
		}
	}

	// update withdrawer balance
	if err := withdrawer.AddBalance(bucket.StakedAmount); err != nil {
		return log, nil, errors.Wrapf(err, "failed to add balance %s", bucket.StakedAmount)
	}
	// put updated withdrawer's account state to trie
	if err := accountutil.StoreAccount(csm.SM(), actionCtx.Caller, withdrawer); err != nil {
		return log, nil, errors.Wrapf(err, "failed to store account %s", actionCtx.Caller.String())
	}

	log.AddAddress(actionCtx.Caller)
	if featureCtx.CannotUnstakeAgain {
		log.SetData(bucket.StakedAmount.Bytes())
	}

	return log, []*action.TransactionLog{
		{
			Type:      iotextypes.TransactionLogType_WITHDRAW_BUCKET,
			Sender:    address.StakingBucketPoolAddr,
			Recipient: actionCtx.Caller.String(),
			Amount:    bucket.StakedAmount,
		},
	}, nil
}

func (p *Protocol) handleChangeCandidate(ctx context.Context, act *action.ChangeCandidate, csm CandidateStateManager,
) (*receiptLog, error) {
	actionCtx := protocol.MustGetActionCtx(ctx)
	featureCtx := protocol.MustGetFeatureCtx(ctx)
	blkCtx := protocol.MustGetBlockCtx(ctx)
	log := newReceiptLog(p.addr.String(), HandleChangeCandidate, featureCtx.NewStakingReceiptFormat)

	_, fetchErr := fetchCaller(ctx, csm, big.NewInt(0))
	if fetchErr != nil {
		return log, fetchErr
	}

	candidate := csm.GetByName(act.Candidate())
	if candidate == nil {
		return log, errCandNotExist
	}

	bucket, fetchErr := p.fetchBucketAndValidate(featureCtx, csm, actionCtx.Caller, act.BucketIndex(), true, false)
	if fetchErr != nil {
		return log, fetchErr
	}
	if !featureCtx.DisableDelegateEndorsement {
		if rErr := validateBucketWithoutEndorsement(ctx, NewEndorsementStateManager(csm.SM()), bucket, blkCtx.BlockHeight); rErr != nil {
			return log, rErr
		}
	}
	log.AddTopics(byteutil.Uint64ToBytesBigEndian(bucket.Index), bucket.Candidate.Bytes(), candidate.GetIdentifier().Bytes())

	prevCandidate := csm.GetByIdentifier(bucket.Candidate)
	if prevCandidate == nil {
		return log, errCandNotExist
	}

	if featureCtx.CannotUnstakeAgain && bucket.isUnstaked() {
		return log, &handleError{
			err:           errors.New("change candidate for an unstaked bucket not allowed"),
			failureStatus: iotextypes.ReceiptStatus_ErrInvalidBucketType,
		}
	}

	if featureCtx.CannotTranferToSelf && address.Equal(prevCandidate.GetIdentifier(), candidate.GetIdentifier()) {
		// change to same candidate, do nothing
		return log, &handleError{
			err:           errors.New("change to same candidate"),
			failureStatus: iotextypes.ReceiptStatus_ErrCandidateAlreadyExist,
		}
	}

	// update bucket index
	if err := csm.delCandBucketIndex(bucket.Candidate, act.BucketIndex()); err != nil {
		return log, errors.Wrapf(err, "failed to delete candidate bucket index for candidate %s", bucket.Candidate.String())
	}
	if err := csm.putCandBucketIndex(candidate.GetIdentifier(), act.BucketIndex()); err != nil {
		return log, errors.Wrapf(err, "failed to put candidate bucket index for candidate %s", candidate.GetIdentifier().String())
	}
	// update bucket
	bucket.Candidate = candidate.GetIdentifier()
	if err := csm.updateBucket(act.BucketIndex(), bucket); err != nil {
		return log, errors.Wrapf(err, "failed to update bucket for voter %s", bucket.Owner.String())
	}

	// update previous candidate
	weightedVotes := p.calculateVoteWeight(bucket, false)
	if err := prevCandidate.SubVote(weightedVotes); err != nil {
		return log, &handleError{
			err:           errors.Wrapf(err, "failed to subtract vote for previous candidate %s", prevCandidate.GetIdentifier().String()),
			failureStatus: iotextypes.ReceiptStatus_ErrNotEnoughBalance,
		}
	}
	// if the bucket equals to the previous candidate's self-stake bucket, it must be expired endorse bucket
	// so we need to clear the self-stake of the previous candidate
	if !featureCtx.DisableDelegateEndorsement && prevCandidate.SelfStakeBucketIdx == bucket.Index {
		prevCandidate.SelfStake.SetInt64(0)
		prevCandidate.SelfStakeBucketIdx = candidateNoSelfStakeBucketIndex
	}
	if err := csm.Upsert(prevCandidate); err != nil {
		return log, csmErrorToHandleError(prevCandidate.GetIdentifier().String(), err)
	}

	// update current candidate
	if err := candidate.AddVote(weightedVotes); err != nil {
		return log, &handleError{
			err:           errors.Wrapf(err, "failed to add vote for candidate %s", candidate.GetIdentifier().String()),
			failureStatus: iotextypes.ReceiptStatus_ErrInvalidBucketAmount,
		}
	}
	if err := csm.Upsert(candidate); err != nil {
		return log, csmErrorToHandleError(candidate.GetIdentifier().String(), err)
	}

	log.AddAddress(candidate.GetIdentifier())
	log.AddAddress(actionCtx.Caller)
	return log, nil
}

func (p *Protocol) handleTransferStake(ctx context.Context, act *action.TransferStake, csm CandidateStateManager,
) (*receiptLog, error) {
	actionCtx := protocol.MustGetActionCtx(ctx)
	featureCtx := protocol.MustGetFeatureCtx(ctx)
	log := newReceiptLog(p.addr.String(), HandleTransferStake, featureCtx.NewStakingReceiptFormat)

	_, fetchErr := fetchCaller(ctx, csm, big.NewInt(0))
	if fetchErr != nil {
		return log, fetchErr
	}

	newOwner := act.VoterAddress()
	bucket, fetchErr := p.fetchBucketAndValidate(featureCtx, csm, actionCtx.Caller, act.BucketIndex(), true, false)
	if fetchErr != nil {
		if featureCtx.ReturnFetchError ||
			fetchErr.ReceiptStatus() != uint64(iotextypes.ReceiptStatus_ErrUnauthorizedOperator) {
			return log, fetchErr
		}

		// check whether the payload contains a valid consignment transfer
		if consignment, ok := p.handleConsignmentTransfer(csm, ctx, act, bucket); ok {
			newOwner = consignment.Transferee()
		} else {
			return log, fetchErr
		}
	}
	log.AddTopics(byteutil.Uint64ToBytesBigEndian(bucket.Index), act.VoterAddress().Bytes(), bucket.Candidate.Bytes())

	if featureCtx.CannotTranferToSelf && address.Equal(newOwner, bucket.Owner) {
		// change to same owner, do nothing
		return log, &handleError{
			err:           errors.New("transfer to same owner"),
			failureStatus: iotextypes.ReceiptStatus_ErrInvalidBucketType,
		}
	}

	// update bucket index
	if err := csm.delVoterBucketIndex(bucket.Owner, act.BucketIndex()); err != nil {
		return log, errors.Wrapf(err, "failed to delete voter bucket index for voter %s", bucket.Owner.String())
	}
	if err := csm.putVoterBucketIndex(newOwner, act.BucketIndex()); err != nil {
		return log, errors.Wrapf(err, "failed to put candidate bucket index for voter %s", act.VoterAddress().String())
	}

	// update bucket
	bucket.Owner = newOwner
	if err := csm.updateBucket(act.BucketIndex(), bucket); err != nil {
		return log, errors.Wrapf(err, "failed to update bucket for voter %s", bucket.Owner.String())
	}

	log.AddAddress(actionCtx.Caller)
	return log, nil
}

func (p *Protocol) handleConsignmentTransfer(
	csm CandidateStateManager,
	ctx context.Context,
	act *action.TransferStake,
	bucket *VoteBucket) (action.Consignment, bool) {
	if len(act.Payload()) == 0 {
		return nil, false
	}

	// self-stake cannot be transferred
	actCtx := protocol.MustGetActionCtx(ctx)
	featureCtx := protocol.MustGetFeatureCtx(ctx)
	selfStake, err := isSelfStakeBucket(featureCtx, csm, bucket)
	if err != nil || selfStake {
		return nil, false
	}

	con, err := action.NewConsignment(act.Payload())
	if err != nil {
		return nil, false
	}

	// a consignment transfer is valid if:
	// (1) signer owns the bucket
	// (2) designated transferee matches the action caller
	// (3) designated asset ID matches bucket index
	// (4) nonce matches the action caller's nonce
	return con, address.Equal(con.Transferor(), bucket.Owner) &&
		address.Equal(con.Transferee(), actCtx.Caller) &&
		con.AssetID() == bucket.Index &&
		con.TransfereeNonce() == actCtx.Nonce
}

func (p *Protocol) handleDepositToStake(ctx context.Context, act *action.DepositToStake, csm CandidateStateManager,
) (*receiptLog, []*action.TransactionLog, error) {
	actionCtx := protocol.MustGetActionCtx(ctx)
	featureCtx := protocol.MustGetFeatureCtx(ctx)
	log := newReceiptLog(p.addr.String(), HandleDepositToStake, featureCtx.NewStakingReceiptFormat)

	depositor, fetchErr := fetchCaller(ctx, csm, act.Amount())
	if fetchErr != nil {
		return log, nil, fetchErr
	}

	bucket, fetchErr := p.fetchBucketAndValidate(featureCtx, csm, actionCtx.Caller, act.BucketIndex(), false, true)
	if fetchErr != nil {
		return log, nil, fetchErr
	}
	log.AddTopics(byteutil.Uint64ToBytesBigEndian(bucket.Index), bucket.Owner.Bytes(), bucket.Candidate.Bytes())
	if !bucket.AutoStake {
		return log, nil, &handleError{
			err:           errors.New("deposit is only allowed on auto-stake bucket"),
			failureStatus: iotextypes.ReceiptStatus_ErrInvalidBucketType,
		}
	}
	candidate := csm.GetByIdentifier(bucket.Candidate)
	if candidate == nil {
		return log, nil, errCandNotExist
	}

	if featureCtx.CannotUnstakeAgain && bucket.isUnstaked() {
		return log, nil, &handleError{
			err:           errors.New("deposit to an unstaked bucket not allowed"),
			failureStatus: iotextypes.ReceiptStatus_ErrInvalidBucketType,
		}
	}
	selfStake, err := isSelfStakeBucket(featureCtx, csm, bucket)
	if err != nil {
		return log, nil, &handleError{
			err:           err,
			failureStatus: iotextypes.ReceiptStatus_ErrUnknown,
		}
	}
	prevWeightedVotes := p.calculateVoteWeight(bucket, selfStake)
	// update bucket
	bucket.StakedAmount.Add(bucket.StakedAmount, act.Amount())
	if err := csm.updateBucket(act.BucketIndex(), bucket); err != nil {
		return log, nil, errors.Wrapf(err, "failed to update bucket for voter %s", bucket.Owner.String())
	}

	// update candidate
	if err := candidate.SubVote(prevWeightedVotes); err != nil {
		return log, nil, &handleError{
			err:           errors.Wrapf(err, "failed to subtract vote for candidate %s", bucket.Candidate.String()),
			failureStatus: iotextypes.ReceiptStatus_ErrNotEnoughBalance,
		}
	}
	weightedVotes := p.calculateVoteWeight(bucket, selfStake)
	if err := candidate.AddVote(weightedVotes); err != nil {
		return log, nil, &handleError{
			err:           errors.Wrapf(err, "failed to add vote for candidate %s", candidate.GetIdentifier().String()),
			failureStatus: iotextypes.ReceiptStatus_ErrInvalidBucketAmount,
		}
	}
	if selfStake {
		if err := candidate.AddSelfStake(act.Amount()); err != nil {
			return log, nil, &handleError{
				err:           errors.Wrapf(err, "failed to add self stake for candidate %s", candidate.GetIdentifier().String()),
				failureStatus: iotextypes.ReceiptStatus_ErrInvalidBucketAmount,
			}
		}
	}
	if err := csm.Upsert(candidate); err != nil {
		return log, nil, csmErrorToHandleError(candidate.GetIdentifier().String(), err)
	}

	// update bucket pool
	if err := csm.DebitBucketPool(act.Amount(), false); err != nil {
		return log, nil, &handleError{
			err:           errors.Wrapf(err, "failed to update staking bucket pool %s", err.Error()),
			failureStatus: iotextypes.ReceiptStatus_ErrWriteAccount,
		}
	}

	// update depositor balance
	if err := depositor.SubBalance(act.Amount()); err != nil {
		return log, nil, &handleError{
			err:           errors.Wrapf(err, "failed to update the balance of depositor %s", actionCtx.Caller.String()),
			failureStatus: iotextypes.ReceiptStatus_ErrNotEnoughBalance,
		}
	}
	// put updated depositor's account state to trie
	if err := accountutil.StoreAccount(csm.SM(), actionCtx.Caller, depositor); err != nil {
		return log, nil, errors.Wrapf(err, "failed to store account %s", actionCtx.Caller.String())
	}
	log.AddAddress(actionCtx.Caller)

	return log, []*action.TransactionLog{
		{
			Type:      iotextypes.TransactionLogType_DEPOSIT_TO_BUCKET,
			Sender:    actionCtx.Caller.String(),
			Recipient: address.StakingBucketPoolAddr,
			Amount:    act.Amount(),
		},
	}, nil
}

func (p *Protocol) handleRestake(ctx context.Context, act *action.Restake, csm CandidateStateManager,
) (*receiptLog, error) {
	actionCtx := protocol.MustGetActionCtx(ctx)
	blkCtx := protocol.MustGetBlockCtx(ctx)
	featureCtx := protocol.MustGetFeatureCtx(ctx)
	log := newReceiptLog(p.addr.String(), HandleRestake, featureCtx.NewStakingReceiptFormat)

	_, fetchErr := fetchCaller(ctx, csm, big.NewInt(0))
	if fetchErr != nil {
		return log, fetchErr
	}

	bucket, fetchErr := p.fetchBucketAndValidate(featureCtx, csm, actionCtx.Caller, act.BucketIndex(), true, true)
	if fetchErr != nil {
		return log, fetchErr
	}
	log.AddTopics(byteutil.Uint64ToBytesBigEndian(bucket.Index), bucket.Candidate.Bytes())

	candidate := csm.GetByIdentifier(bucket.Candidate)
	if candidate == nil {
		return log, errCandNotExist
	}

	if featureCtx.CannotUnstakeAgain && bucket.isUnstaked() {
		return log, &handleError{
			err:           errors.New("restake an unstaked bucket not allowed"),
			failureStatus: iotextypes.ReceiptStatus_ErrInvalidBucketType,
		}
	}
	selfStake, err := isSelfStakeBucket(featureCtx, csm, bucket)
	if err != nil {
		return log, &handleError{
			err:           err,
			failureStatus: iotextypes.ReceiptStatus_ErrUnknown,
		}
	}
	prevWeightedVotes := p.calculateVoteWeight(bucket, selfStake)
	// update bucket
	actDuration := time.Duration(act.Duration()) * 24 * time.Hour
	if bucket.StakedDuration.Hours() > actDuration.Hours() {
		// in case of reducing the duration
		if bucket.AutoStake {
			// if auto-stake on, user can't reduce duration
			return log, &handleError{
				err:           errors.New("AutoStake should be disabled first in order to reduce duration"),
				failureStatus: iotextypes.ReceiptStatus_ErrInvalidBucketType,
			}
		} else if blkCtx.BlockTimeStamp.Before(bucket.StakeStartTime.Add(bucket.StakedDuration)) {
			// if auto-stake off and maturity is not enough
			return log, &handleError{
				err:           errors.New("bucket is not ready to be able to reduce duration"),
				failureStatus: iotextypes.ReceiptStatus_ErrReduceDurationBeforeMaturity,
			}
		}
	}
	bucket.StakedDuration = actDuration
	bucket.StakeStartTime = blkCtx.BlockTimeStamp.UTC()
	bucket.AutoStake = act.AutoStake()
	if err := csm.updateBucket(act.BucketIndex(), bucket); err != nil {
		return log, errors.Wrapf(err, "failed to update bucket for voter %s", bucket.Owner.String())
	}

	// update candidate
	if err := candidate.SubVote(prevWeightedVotes); err != nil {
		return log, &handleError{
			err:           errors.Wrapf(err, "failed to subtract vote for candidate %s", bucket.Candidate.String()),
			failureStatus: iotextypes.ReceiptStatus_ErrNotEnoughBalance,
		}
	}
	weightedVotes := p.calculateVoteWeight(bucket, selfStake)
	if err := candidate.AddVote(weightedVotes); err != nil {
		return log, &handleError{
			err:           errors.Wrapf(err, "failed to add vote for candidate %s", candidate.GetIdentifier().String()),
			failureStatus: iotextypes.ReceiptStatus_ErrInvalidBucketAmount,
		}
	}
	if err := csm.Upsert(candidate); err != nil {
		return log, csmErrorToHandleError(candidate.GetIdentifier().String(), err)
	}

	log.AddAddress(actionCtx.Caller)
	return log, nil
}

func (p *Protocol) handleCandidateRegister(ctx context.Context, act *action.CandidateRegister, csm CandidateStateManager,
) (*receiptLog, []*action.TransactionLog, error) {
	actCtx := protocol.MustGetActionCtx(ctx)
	blkCtx := protocol.MustGetBlockCtx(ctx)
	featureCtx := protocol.MustGetFeatureCtx(ctx)
	log := newReceiptLog(p.addr.String(), HandleCandidateRegister, featureCtx.NewStakingReceiptFormat)

	registrationFee := new(big.Int).Set(p.config.RegistrationConsts.Fee)

	caller, fetchErr := fetchCaller(ctx, csm, new(big.Int).Add(act.Amount(), registrationFee))
	if fetchErr != nil {
		return log, nil, fetchErr
	}

	owner := actCtx.Caller
	if act.OwnerAddress() != nil {
		owner = act.OwnerAddress()
	}

	c := csm.GetByOwner(owner)
	ownerExist := c != nil
	// cannot collide with existing owner (with selfstake != 0)
	if ownerExist {
		if !featureCtx.CandidateIdentifiedByOwner || (featureCtx.CandidateIdentifiedByOwner && c.SelfStake.Cmp(big.NewInt(0)) != 0) {
			return log, nil, &handleError{
				err:           ErrInvalidOwner,
				failureStatus: iotextypes.ReceiptStatus_ErrCandidateAlreadyExist,
			}
		}
	}
	// cannot collide with existing identifier
	candID := owner
	if !featureCtx.CandidateIdentifiedByOwner {
		// cannot collide with existing identifier
		if csm.GetByIdentifier(owner) != nil {
			return log, nil, &handleError{
				err:           ErrInvalidOwner,
				failureStatus: iotextypes.ReceiptStatus_ErrCandidateAlreadyExist,
			}
		}
		id, err := p.generateCandidateID(owner, blkCtx.BlockHeight, csm)
		if err != nil {
			return log, nil, &handleError{
				err:           errors.Wrap(err, "failed to generate candidate ID"),
				failureStatus: iotextypes.ReceiptStatus_ErrCandidateAlreadyExist,
			}
		}
		candID = id
	}
	// cannot collide with existing name
	if csm.ContainsName(act.Name()) && (!ownerExist || act.Name() != c.Name) {
		return log, nil, &handleError{
			err:           action.ErrInvalidCanName,
			failureStatus: iotextypes.ReceiptStatus_ErrCandidateConflict,
		}
	}
	// cannot collide with existing operator address
	if csm.ContainsOperator(act.OperatorAddress()) &&
		(!ownerExist || !address.Equal(act.OperatorAddress(), c.Operator)) {
		return log, nil, &handleError{
			err:           ErrInvalidOperator,
			failureStatus: iotextypes.ReceiptStatus_ErrCandidateConflict,
		}
	}

	var (
		bucketIdx     uint64
		votes         *big.Int
		withSelfStake = act.Amount().Sign() > 0
		txLogs        []*action.TransactionLog
		err           error
	)
	if withSelfStake {
		// register with self-stake
		bucket := NewVoteBucket(candID, owner, act.Amount(), act.Duration(), blkCtx.BlockTimeStamp, act.AutoStake())
		bucketIdx, err = csm.putBucketAndIndex(bucket)
		if err != nil {
			return log, nil, err
		}
		txLogs = append(txLogs, &action.TransactionLog{
			Type:      iotextypes.TransactionLogType_CANDIDATE_SELF_STAKE,
			Sender:    actCtx.Caller.String(),
			Recipient: address.StakingBucketPoolAddr,
			Amount:    act.Amount(),
		})
		votes = p.calculateVoteWeight(bucket, true)
	} else {
		// register w/o self-stake, waiting to be endorsed
		bucketIdx = uint64(candidateNoSelfStakeBucketIndex)
		votes = big.NewInt(0)
	}
	log.AddTopics(byteutil.Uint64ToBytesBigEndian(bucketIdx), candID.Bytes())

	c = &Candidate{
		Owner:              owner,
		Operator:           act.OperatorAddress(),
		Reward:             act.RewardAddress(),
		Name:               act.Name(),
		Votes:              votes,
		SelfStakeBucketIdx: bucketIdx,
		SelfStake:          act.Amount(),
	}
	if !featureCtx.CandidateIdentifiedByOwner {
		c.Identifier = candID
	}

	if err := csm.Upsert(c); err != nil {
		return log, nil, csmErrorToHandleError(owner.String(), err)
	}
	height, _ := csm.SM().Height()
	if p.needToWriteCandsMap(ctx, height) {
		csm.DirtyView().candCenter.base.recordOwner(c)
	}

	if withSelfStake {
		// update bucket pool
		if err := csm.DebitBucketPool(act.Amount(), true); err != nil {
			return log, nil, &handleError{
				err:           errors.Wrapf(err, "failed to update staking bucket pool %s", err.Error()),
				failureStatus: iotextypes.ReceiptStatus_ErrWriteAccount,
			}
		}
		// update caller balance
		if err := caller.SubBalance(act.Amount()); err != nil {
			return log, nil, &handleError{
				err:           errors.Wrapf(err, "failed to update the balance of register %s", actCtx.Caller.String()),
				failureStatus: iotextypes.ReceiptStatus_ErrNotEnoughBalance,
			}
		}
		// put updated caller's account state to trie
		if err := accountutil.StoreAccount(csm.SM(), actCtx.Caller, caller); err != nil {
			return log, nil, errors.Wrapf(err, "failed to store account %s", actCtx.Caller.String())
		}
	}

	// put registrationFee to reward pool
	if _, err := p.helperCtx.DepositGas(ctx, csm.SM(), registrationFee); err != nil {
		return log, nil, errors.Wrap(err, "failed to deposit gas")
	}

	log.AddAddress(candID)
	log.AddAddress(actCtx.Caller)
	log.SetData(byteutil.Uint64ToBytesBigEndian(bucketIdx))

	txLogs = append(txLogs, &action.TransactionLog{
		Type:      iotextypes.TransactionLogType_CANDIDATE_REGISTRATION_FEE,
		Sender:    actCtx.Caller.String(),
		Recipient: address.RewardingPoolAddr,
		Amount:    registrationFee,
	})
	return log, txLogs, nil
}

func (p *Protocol) handleCandidateUpdate(ctx context.Context, act *action.CandidateUpdate, csm CandidateStateManager,
) (*receiptLog, error) {
	actCtx := protocol.MustGetActionCtx(ctx)
	featureCtx := protocol.MustGetFeatureCtx(ctx)
	log := newReceiptLog(p.addr.String(), HandleCandidateUpdate, featureCtx.NewStakingReceiptFormat)

	_, fetchErr := fetchCaller(ctx, csm, big.NewInt(0))
	if fetchErr != nil {
		return log, fetchErr
	}

	// only owner can update candidate
	c := csm.GetByOwner(actCtx.Caller)
	if c == nil {
		return log, errCandNotExist
	}

	if len(act.Name()) != 0 {
		c.Name = act.Name()
	}

	if act.OperatorAddress() != nil {
		c.Operator = act.OperatorAddress()
	}

	if act.RewardAddress() != nil {
		c.Reward = act.RewardAddress()
	}
	log.AddTopics(c.GetIdentifier().Bytes())

	if err := csm.Upsert(c); err != nil {
		return log, csmErrorToHandleError(c.GetIdentifier().String(), err)
	}
	height, _ := csm.SM().Height()
	if p.needToWriteCandsMap(ctx, height) {
		csm.DirtyView().candCenter.base.recordOwner(c)
	}

	log.AddAddress(actCtx.Caller)
	return log, nil
}

func (p *Protocol) fetchBucket(csm BucketGetByIndex, index uint64) (*VoteBucket, ReceiptError) {
	bucket, err := csm.getBucket(index)
	if err != nil {
		fetchErr := &handleError{
			err:           errors.Wrapf(err, "failed to fetch bucket by index %d", index),
			failureStatus: iotextypes.ReceiptStatus_Failure,
		}
		if errors.Cause(err) == state.ErrStateNotExist {
			fetchErr.failureStatus = iotextypes.ReceiptStatus_ErrInvalidBucketIndex
		}
		return nil, fetchErr
	}
	return bucket, nil
}

func (p *Protocol) fetchBucketAndValidate(
	featureCtx protocol.FeatureCtx,
	csm CandidateStateManager,
	caller address.Address,
	index uint64,
	checkOwner bool,
	allowSelfStaking bool,
) (*VoteBucket, ReceiptError) {
	bucket, err := p.fetchBucket(csm, index)
	if err != nil {
		return nil, err
	}

	// ReceiptStatus_ErrUnauthorizedOperator indicates action caller is not bucket owner
	// upon return, the action will be subject to check whether it contains a valid consignment transfer
	// do NOT return this value in case changes are added in the future
	if checkOwner && !address.Equal(bucket.Owner, caller) {
		return bucket, &handleError{
			err:           errors.New("bucket owner does not match action caller"),
			failureStatus: iotextypes.ReceiptStatus_ErrUnauthorizedOperator,
		}
	}
	if !allowSelfStaking {
		selfStaking, serr := isSelfStakeBucket(featureCtx, csm, bucket)
		if serr != nil {
			return bucket, &handleError{
				err:           serr,
				failureStatus: iotextypes.ReceiptStatus_ErrUnknown,
			}
		}
		if selfStaking {
			return bucket, &handleError{
				err:           errors.New("self staking bucket cannot be processed"),
				failureStatus: iotextypes.ReceiptStatus_ErrInvalidBucketType,
			}
		}
	}
	return bucket, nil
}

func (p *Protocol) generateCandidateID(owner address.Address, height uint64, csm CandidateStateManager) (address.Address, error) {
	isValidID := func(id address.Address) bool {
		return csm.GetByIdentifier(id) == nil && csm.GetByOwner(id) == nil
	}
	if isValidID(owner) {
		return owner, nil
	}
	h := append(owner.Bytes(), byteutil.Uint64ToBytesBigEndian(height)...)
	for i := 0; i < 1000; i++ {
		b := hash.Hash160b(append(h, byteutil.Uint64ToBytesBigEndian(uint64(i))...))
		addr, err := address.FromBytes(b[:])
		if err != nil {
			return nil, errors.Wrap(err, "failed to generate candidate ID")
		}
		if isValidID(addr) {
			return addr, nil
		}
	}
	return nil, errors.New("failed to generate candidate ID after max attempts")
}

func fetchCaller(ctx context.Context, csm CandidateStateManager, amount *big.Int) (*state.Account, ReceiptError) {
	actionCtx := protocol.MustGetActionCtx(ctx)
	accountCreationOpts := []state.AccountCreationOption{}
	if protocol.MustGetFeatureCtx(ctx).CreateLegacyNonceAccount {
		accountCreationOpts = append(accountCreationOpts, state.LegacyNonceAccountTypeOption())
	}
	caller, err := accountutil.LoadAccount(csm.SM(), actionCtx.Caller, accountCreationOpts...)
	if err != nil {
		return nil, &handleError{
			err:           errors.Wrapf(err, "failed to load the account of caller %s", actionCtx.Caller.String()),
			failureStatus: iotextypes.ReceiptStatus_Failure,
		}
	}
	gasFee := big.NewInt(0).Mul(actionCtx.GasPrice, big.NewInt(0).SetUint64(actionCtx.IntrinsicGas))
	// check caller's balance
	if !caller.HasSufficientBalance(new(big.Int).Add(amount, gasFee)) {
		return nil, &handleError{
			err:           errors.Wrapf(state.ErrNotEnoughBalance, "caller %s balance not enough", actionCtx.Caller.String()),
			failureStatus: iotextypes.ReceiptStatus_ErrNotEnoughBalance,
		}
	}
	return caller, nil
}

func csmErrorToHandleError(caller string, err error) error {
	hErr := &handleError{
		err: errors.Wrapf(err, "failed to put candidate %s", caller),
	}

	switch errors.Cause(err) {
	case action.ErrInvalidCanName:
		hErr.failureStatus = iotextypes.ReceiptStatus_ErrCandidateConflict
		return hErr
	case ErrInvalidOperator:
		hErr.failureStatus = iotextypes.ReceiptStatus_ErrCandidateConflict
		return hErr
	case ErrInvalidSelfStkIndex:
		hErr.failureStatus = iotextypes.ReceiptStatus_ErrCandidateConflict
		return hErr
	case action.ErrInvalidAmount:
		hErr.failureStatus = iotextypes.ReceiptStatus_ErrCandidateNotExist
		return hErr
	case ErrInvalidOwner:
		hErr.failureStatus = iotextypes.ReceiptStatus_ErrCandidateNotExist
		return hErr
	case ErrInvalidReward:
		hErr.failureStatus = iotextypes.ReceiptStatus_ErrCandidateNotExist
		return hErr
	default:
		return err
	}
}

// BucketIndexFromReceiptLog extracts bucket index from log
func BucketIndexFromReceiptLog(log *iotextypes.Log) (uint64, bool) {
	if log == nil || len(log.Topics) < 2 {
		return 0, false
	}

	h := hash.Hash160b([]byte(_protocolID))
	addr, _ := address.FromBytes(h[:])
	if log.ContractAddress != addr.String() {
		return 0, false
	}

	switch hash.BytesToHash256(log.Topics[0]) {
	case hash.BytesToHash256([]byte(HandleCreateStake)), hash.BytesToHash256([]byte(HandleUnstake)),
		hash.BytesToHash256([]byte(HandleWithdrawStake)), hash.BytesToHash256([]byte(HandleChangeCandidate)),
		hash.BytesToHash256([]byte(HandleTransferStake)), hash.BytesToHash256([]byte(HandleDepositToStake)),
		hash.BytesToHash256([]byte(HandleRestake)), hash.BytesToHash256([]byte(HandleCandidateRegister)):
		return byteutil.BytesToUint64BigEndian(log.Topics[1][24:]), true
	default:
		return 0, false
	}
}
