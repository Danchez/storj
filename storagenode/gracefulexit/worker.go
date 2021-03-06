// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package gracefulexit

import (
	"bytes"
	"context"
	"io"
	"os"
	"time"

	"github.com/zeebo/errs"
	"go.uber.org/zap"

	"storj.io/common/errs2"
	"storj.io/common/memory"
	"storj.io/common/pb"
	"storj.io/common/rpc"
	"storj.io/common/rpc/rpcstatus"
	"storj.io/common/signing"
	"storj.io/common/storj"
	"storj.io/common/sync2"
	"storj.io/storj/storagenode/pieces"
	"storj.io/storj/storagenode/piecestore"
	"storj.io/storj/storagenode/satellites"
	"storj.io/uplink/private/ecclient"
)

// Worker is responsible for completing the graceful exit for a given satellite.
type Worker struct {
	log                *zap.Logger
	store              *pieces.Store
	satelliteDB        satellites.DB
	dialer             rpc.Dialer
	limiter            *sync2.Limiter
	satelliteID        storj.NodeID
	satelliteAddr      string
	ecclient           ecclient.Client
	minBytesPerSecond  memory.Size
	minDownloadTimeout time.Duration
}

// NewWorker instantiates Worker.
func NewWorker(log *zap.Logger, store *pieces.Store, satelliteDB satellites.DB, dialer rpc.Dialer, satelliteID storj.NodeID, satelliteAddr string, config Config) *Worker {
	return &Worker{
		log:                log,
		store:              store,
		satelliteDB:        satelliteDB,
		dialer:             dialer,
		limiter:            sync2.NewLimiter(config.NumConcurrentTransfers),
		satelliteID:        satelliteID,
		satelliteAddr:      satelliteAddr,
		ecclient:           ecclient.NewClient(log, dialer, 0),
		minBytesPerSecond:  config.MinBytesPerSecond,
		minDownloadTimeout: config.MinDownloadTimeout,
	}
}

// Run calls the satellite endpoint, transfers pieces, validates, and responds with success or failure.
// It also marks the satellite finished once all the pieces have been transferred
func (worker *Worker) Run(ctx context.Context, done func()) (err error) {
	defer mon.Task()(&ctx)(&err)
	defer done()

	worker.log.Debug("running worker")

	conn, err := worker.dialer.DialAddressID(ctx, worker.satelliteAddr, worker.satelliteID)
	if err != nil {
		return errs.Wrap(err)
	}
	defer func() {
		err = errs.Combine(err, conn.Close())
	}()

	client := pb.NewDRPCSatelliteGracefulExitClient(conn)

	c, err := client.Process(ctx)
	if err != nil {
		return errs.Wrap(err)
	}

	for {
		response, err := c.Recv()
		if errs.Is(err, io.EOF) {
			// Done
			return nil
		}
		if errs2.IsRPC(err, rpcstatus.FailedPrecondition) {
			// delete the entry from satellite table and inform graceful exit has failed to start
			deleteErr := worker.satelliteDB.CancelGracefulExit(ctx, worker.satelliteID)
			if deleteErr != nil {
				// TODO: what to do now?
				return errs.Combine(deleteErr, err)
			}
			return errs.Wrap(err)
		}
		if err != nil {
			// TODO what happened
			return errs.Wrap(err)
		}

		switch msg := response.GetMessage().(type) {
		case *pb.SatelliteMessage_NotReady:
			return nil

		case *pb.SatelliteMessage_TransferPiece:
			transferPieceMsg := msg.TransferPiece
			worker.limiter.Go(ctx, func() {
				err = worker.transferPiece(ctx, transferPieceMsg, c)
				if err != nil {
					worker.log.Error("failed to transfer piece.",
						zap.Stringer("Satellite ID", worker.satelliteID),
						zap.Error(errs.Wrap(err)))
				}
			})

		case *pb.SatelliteMessage_DeletePiece:
			deletePieceMsg := msg.DeletePiece
			worker.limiter.Go(ctx, func() {
				pieceID := deletePieceMsg.OriginalPieceId
				err := worker.deleteOnePiece(ctx, pieceID)
				if err != nil {
					worker.log.Error("failed to delete piece.",
						zap.Stringer("Satellite ID", worker.satelliteID),
						zap.Stringer("Piece ID", pieceID),
						zap.Error(errs.Wrap(err)))
				}
			})

		case *pb.SatelliteMessage_ExitFailed:
			worker.log.Error("graceful exit failed.",
				zap.Stringer("Satellite ID", worker.satelliteID),
				zap.Stringer("reason", msg.ExitFailed.Reason))

			exitFailedBytes, err := pb.Marshal(msg.ExitFailed)
			if err != nil {
				worker.log.Error("failed to marshal exit failed message.")
			}
			err = worker.satelliteDB.CompleteGracefulExit(ctx, worker.satelliteID, time.Now(), satellites.ExitFailed, exitFailedBytes)
			return errs.Wrap(err)

		case *pb.SatelliteMessage_ExitCompleted:
			worker.log.Info("graceful exit completed.", zap.Stringer("Satellite ID", worker.satelliteID))

			exitCompletedBytes, err := pb.Marshal(msg.ExitCompleted)
			if err != nil {
				worker.log.Error("failed to marshal exit completed message.")
			}

			err = worker.satelliteDB.CompleteGracefulExit(ctx, worker.satelliteID, time.Now(), satellites.ExitSucceeded, exitCompletedBytes)
			if err != nil {
				return errs.Wrap(err)
			}
			// delete all remaining pieces
			err = worker.deleteAllPieces(ctx)
			return errs.Wrap(err)

		default:
			// TODO handle err
			worker.log.Error("unknown graceful exit message.", zap.Stringer("Satellite ID", worker.satelliteID))
		}
	}
}

type gracefulExitStream interface {
	Context() context.Context
	Send(*pb.StorageNodeMessage) error
	Recv() (*pb.SatelliteMessage, error)
}

func (worker *Worker) transferPiece(ctx context.Context, transferPiece *pb.TransferPiece, c gracefulExitStream) error {
	pieceID := transferPiece.OriginalPieceId
	reader, err := worker.store.Reader(ctx, worker.satelliteID, pieceID)
	if err != nil {
		transferErr := pb.TransferFailed_UNKNOWN
		if errs.Is(err, os.ErrNotExist) {
			transferErr = pb.TransferFailed_NOT_FOUND
		}
		worker.log.Error("failed to get piece reader.",
			zap.Stringer("Satellite ID", worker.satelliteID),
			zap.Stringer("Piece ID", pieceID),
			zap.Error(errs.Wrap(err)))
		worker.handleFailure(ctx, transferErr, pieceID, c.Send)
		return err
	}

	addrLimit := transferPiece.GetAddressedOrderLimit()
	pk := transferPiece.PrivateKey

	originalHash, originalOrderLimit, err := worker.store.GetHashAndLimit(ctx, worker.satelliteID, pieceID, reader)
	if err != nil {
		worker.log.Error("failed to get piece hash and order limit.",
			zap.Stringer("Satellite ID", worker.satelliteID),
			zap.Stringer("Piece ID", pieceID),
			zap.Error(errs.Wrap(err)))
		worker.handleFailure(ctx, pb.TransferFailed_UNKNOWN, pieceID, c.Send)
		return err
	}

	if worker.minBytesPerSecond == 0 {
		// set minBytesPerSecond to default 128B if set to 0
		worker.minBytesPerSecond = 128 * memory.B
	}
	maxTransferTime := time.Duration(int64(time.Second) * originalHash.PieceSize / worker.minBytesPerSecond.Int64())
	if maxTransferTime < worker.minDownloadTimeout {
		maxTransferTime = worker.minDownloadTimeout
	}
	putCtx, cancel := context.WithTimeout(ctx, maxTransferTime)
	defer cancel()

	pieceHash, peerID, err := worker.ecclient.PutPiece(putCtx, ctx, addrLimit, pk, reader)
	if err != nil {
		if piecestore.ErrVerifyUntrusted.Has(err) {
			worker.log.Error("failed hash verification.",
				zap.Stringer("Satellite ID", worker.satelliteID),
				zap.Stringer("Piece ID", pieceID),
				zap.Error(errs.Wrap(err)))
			worker.handleFailure(ctx, pb.TransferFailed_HASH_VERIFICATION, pieceID, c.Send)
		} else {
			worker.log.Error("failed to put piece.",
				zap.Stringer("Satellite ID", worker.satelliteID),
				zap.Stringer("Piece ID", pieceID),
				zap.Error(errs.Wrap(err)))
			// TODO look at error type to decide on the transfer error
			worker.handleFailure(ctx, pb.TransferFailed_STORAGE_NODE_UNAVAILABLE, pieceID, c.Send)
		}
		return err
	}

	if !bytes.Equal(originalHash.Hash, pieceHash.Hash) {
		worker.log.Error("piece hash from new storagenode does not match",
			zap.Stringer("Storagenode ID", addrLimit.Limit.StorageNodeId),
			zap.Stringer("Satellite ID", worker.satelliteID),
			zap.Stringer("Piece ID", pieceID))
		worker.handleFailure(ctx, pb.TransferFailed_HASH_VERIFICATION, pieceID, c.Send)
		return Error.New("piece hash from new storagenode does not match")
	}
	if pieceHash.PieceId != addrLimit.Limit.PieceId {
		worker.log.Error("piece id from new storagenode does not match order limit",
			zap.Stringer("Storagenode ID", addrLimit.Limit.StorageNodeId),
			zap.Stringer("Satellite ID", worker.satelliteID),
			zap.Stringer("Piece ID", pieceID))
		worker.handleFailure(ctx, pb.TransferFailed_HASH_VERIFICATION, pieceID, c.Send)
		return Error.New("piece id from new storagenode does not match order limit")
	}

	signee := signing.SigneeFromPeerIdentity(peerID)
	err = signing.VerifyPieceHashSignature(ctx, signee, pieceHash)
	if err != nil {
		worker.log.Error("invalid piece hash signature from new storagenode",
			zap.Stringer("Storagenode ID", addrLimit.Limit.StorageNodeId),
			zap.Stringer("Satellite ID", worker.satelliteID),
			zap.Stringer("Piece ID", pieceID),
			zap.Error(errs.Wrap(err)))
		worker.handleFailure(ctx, pb.TransferFailed_HASH_VERIFICATION, pieceID, c.Send)
		return err
	}

	success := &pb.StorageNodeMessage{
		Message: &pb.StorageNodeMessage_Succeeded{
			Succeeded: &pb.TransferSucceeded{
				OriginalPieceId:      transferPiece.OriginalPieceId,
				OriginalPieceHash:    &originalHash,
				OriginalOrderLimit:   &originalOrderLimit,
				ReplacementPieceHash: pieceHash,
			},
		},
	}
	worker.log.Info("piece transferred to new storagenode",
		zap.Stringer("Storagenode ID", addrLimit.Limit.StorageNodeId),
		zap.Stringer("Satellite ID", worker.satelliteID),
		zap.Stringer("Piece ID", pieceID))
	return c.Send(success)
}

// deleteOnePiece deletes one piece stored for a satellite
func (worker *Worker) deleteOnePiece(ctx context.Context, pieceID storj.PieceID) error {
	piece, err := worker.store.Reader(ctx, worker.satelliteID, pieceID)
	if err != nil {
		if !errs2.IsCanceled(err) {
			worker.log.Debug("failed to retrieve piece info", zap.Stringer("Satellite ID", worker.satelliteID),
				zap.Stringer("Piece ID", pieceID), zap.Error(err))
		}
		return err
	}
	err = worker.deletePiece(ctx, pieceID)
	if err != nil {
		worker.log.Debug("failed to retrieve piece info", zap.Stringer("Satellite ID", worker.satelliteID), zap.Error(err))
		return err
	}
	// update graceful exit progress
	size := piece.Size()
	return worker.satelliteDB.UpdateGracefulExit(ctx, worker.satelliteID, size)
}

// deletePiece deletes one piece stored for a satellite, without updating satellite Graceful Exit status
func (worker *Worker) deletePiece(ctx context.Context, pieceID storj.PieceID) error {
	err := worker.store.Delete(ctx, worker.satelliteID, pieceID)
	if err != nil {
		worker.log.Debug("failed to delete a piece",
			zap.Stringer("Satellite ID", worker.satelliteID),
			zap.Stringer("Piece ID", pieceID),
			zap.Error(err))
		delErr := worker.store.DeleteFailed(ctx, pieces.ExpiredInfo{
			SatelliteID: worker.satelliteID,
			PieceID:     pieceID,
			InPieceInfo: true,
		}, time.Now().UTC())
		if delErr != nil {
			worker.log.Debug("failed to mark a deletion failure for a piece",
				zap.Stringer("Satellite ID", worker.satelliteID),
				zap.Stringer("Piece ID", pieceID), zap.Error(err))
		}
		return errs.Combine(err, delErr)
	}
	worker.log.Debug("delete piece",
		zap.Stringer("Satellite ID", worker.satelliteID),
		zap.Stringer("Piece ID", pieceID))
	return err
}

// deleteAllPieces deletes pieces stored for a satellite
func (worker *Worker) deleteAllPieces(ctx context.Context) error {
	var totalDeleted int64
	err := worker.store.WalkSatellitePieces(ctx, worker.satelliteID, func(piece pieces.StoredPieceAccess) error {
		err := worker.deletePiece(ctx, piece.PieceID())
		if err == nil {
			_, size, err := piece.Size(ctx)
			if err != nil {
				worker.log.Debug("failed to retrieve piece info", zap.Stringer("Satellite ID", worker.satelliteID),
					zap.Stringer("Piece ID", piece.PieceID()), zap.Error(err))
			}
			totalDeleted += size
		}
		return err
	})
	if err != nil && !errs2.IsCanceled(err) {
		worker.log.Debug("failed to retrieve piece info", zap.Stringer("Satellite ID", worker.satelliteID), zap.Error(err))
	}
	// update graceful exit progress
	return worker.satelliteDB.UpdateGracefulExit(ctx, worker.satelliteID, totalDeleted)
}

func (worker *Worker) handleFailure(ctx context.Context, transferError pb.TransferFailed_Error, pieceID pb.PieceID, send func(*pb.StorageNodeMessage) error) {
	failure := &pb.StorageNodeMessage{
		Message: &pb.StorageNodeMessage_Failed{
			Failed: &pb.TransferFailed{
				OriginalPieceId: pieceID,
				Error:           transferError,
			},
		},
	}

	sendErr := send(failure)
	if sendErr != nil {
		worker.log.Error("unable to send failure.", zap.Stringer("Satellite ID", worker.satelliteID))
	}
}

// Close halts the worker.
func (worker *Worker) Close() error {
	worker.limiter.Wait()
	return nil
}
