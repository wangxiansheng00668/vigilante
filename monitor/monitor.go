package monitor

import (
	"fmt"
	"github.com/btcsuite/btcd/wire"
	"github.com/pkg/errors"
	"go.uber.org/atomic"
	"sort"
	"sync"

	checkpointingtypes "github.com/babylonchain/babylon/x/checkpointing/types"
	bbnclient "github.com/babylonchain/rpc-client/client"
	"github.com/babylonchain/vigilante/config"
	"github.com/babylonchain/vigilante/monitor/btcscanner"
	"github.com/babylonchain/vigilante/monitor/querier"
	"github.com/babylonchain/vigilante/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
)

type Monitor struct {
	Cfg *config.MonitorConfig

	// BTCScanner scans BTC blocks for checkpoints
	BTCScanner btcscanner.Scanner
	// BBNQuerier queries epoch info from Babylon
	BBNQuerier *querier.Querier

	// curEpoch contains information of the current epoch for verification
	curEpoch *types.EpochInfo

	// tracks checkpoint records that have not been reported back to Babylon
	checkpointChecklist *types.CheckpointsBookkeeper

	wg      sync.WaitGroup
	started *atomic.Bool
	quit    chan struct{}
}

func New(cfg *config.MonitorConfig, genesisInfo *types.GenesisInfo, scanner btcscanner.Scanner, babylonClient bbnclient.BabylonClient) (*Monitor, error) {
	// genesis validator set needs to be sorted by address to respect the signing order
	sortedGenesisValSet := GetSortedValSet(genesisInfo.GetBLSKeySet())
	genesisEpoch := types.NewEpochInfo(
		uint64(0),
		sortedGenesisValSet,
	)

	return &Monitor{
		BBNQuerier:          querier.New(babylonClient),
		BTCScanner:          scanner,
		Cfg:                 cfg,
		curEpoch:            genesisEpoch,
		checkpointChecklist: types.NewCheckpointsBookkeeper(),
		quit:                make(chan struct{}),
		started:             atomic.NewBool(false),
	}, nil
}

// Start starts the verification core
func (m *Monitor) Start() {
	if m.started.Load() {
		log.Info("the Monitor is already started")
		return
	}

	m.started.Store(true)
	log.Info("the Monitor is started")

	// starting BTC scanner
	m.wg.Add(1)
	go m.runBTCScanner()

	if m.Cfg.LivenessChecker {
		// starting liveness checker
		m.wg.Add(1)
		go m.runLivenessChecker()
	}

	for m.started.Load() {
		select {
		case <-m.quit:
			log.Info("the monitor is stopping")
			m.started.Store(false)
		case header := <-m.BTCScanner.GetHeadersChan():
			err := m.handleNewConfirmedHeader(header)
			if err != nil {
				log.Errorf("failed to handle BTC header: %s", err.Error())
				break
			}
		case ckpt := <-m.BTCScanner.GetCheckpointsChan():
			err := m.handleNewConfirmedCheckpoint(ckpt)
			if err != nil {
				log.Errorf("failed to handle BTC raw checkpoint at epoch %d: %s", ckpt.EpochNum(), err.Error())
			}
		}
	}

	m.wg.Wait()
	log.Info("the Monitor is stopped")
}

func (m *Monitor) runBTCScanner() {
	m.BTCScanner.Start()
	m.wg.Done()
}

func (m *Monitor) handleNewConfirmedHeader(header *wire.BlockHeader) error {
	return m.checkHeaderConsistency(header)
}

func (m *Monitor) handleNewConfirmedCheckpoint(ckpt *types.CheckpointRecord) error {
	err := m.VerifyCheckpoint(ckpt.RawCheckpoint)
	if err != nil {
		if sdkerrors.IsOf(err, types.ErrInconsistentLastCommitHash) {
			// also record conflicting checkpoints since we need to ensure that
			// alarm will be sent if conflicting checkpoints are censored
			if m.Cfg.LivenessChecker {
				m.addCheckpointToCheckList(ckpt)
			}
			// stop verification if a valid BTC checkpoint on an inconsistent LastCommitHash is found
			// this means the ledger is on a fork
			return fmt.Errorf("verification failed at epoch %v: %w", m.GetCurrentEpoch(), err)
		}
		// skip the error if it is not ErrInconsistentLastCommitHash and verify the next BTC checkpoint
		log.Infof("invalid BTC checkpoint found at epoch %v: %s", m.GetCurrentEpoch(), err.Error())
		return nil
	}

	if m.Cfg.LivenessChecker {
		m.addCheckpointToCheckList(ckpt)
	}

	log.Infof("checkpoint at epoch %v has passed the verification", m.GetCurrentEpoch())

	nextEpochNum := m.GetCurrentEpoch() + 1
	err = m.UpdateEpochInfo(nextEpochNum)
	if err != nil {
		return fmt.Errorf("failed to update information of epoch %d: %w", nextEpochNum, err)
	}

	return nil
}

func (m *Monitor) GetCurrentEpoch() uint64 {
	return m.curEpoch.GetEpochNumber()
}

// VerifyCheckpoint verifies the BTC checkpoint against the Babylon counterpart
func (m *Monitor) VerifyCheckpoint(btcCkpt *checkpointingtypes.RawCheckpoint) error {
	// check whether the epoch number of the checkpoint equals to the current epoch number
	if m.GetCurrentEpoch() != btcCkpt.EpochNum {
		return errors.Wrapf(types.ErrInvalidEpochNum, fmt.Sprintf("found a checkpoint with epoch %v, but the monitor expects epoch %v",
			btcCkpt.EpochNum, m.GetCurrentEpoch()))
	}
	// verify BLS sig of the BTC checkpoint
	err := m.curEpoch.VerifyMultiSig(btcCkpt)
	if err != nil {
		return fmt.Errorf("invalid BLS sig of BTC checkpoint at epoch %d: %w", m.GetCurrentEpoch(), err)
	}
	// query checkpoint from Babylon
	bbnCkpt, err := m.BBNQuerier.QueryRawCheckpoint(btcCkpt.EpochNum)
	if err != nil {
		return fmt.Errorf("failed to query raw checkpoint from Babylon, epoch %v: %w", btcCkpt.EpochNum, err)
	}
	// verify BLS sig of the raw checkpoint from Babylon
	err = m.curEpoch.VerifyMultiSig(bbnCkpt.Ckpt)
	if err != nil {
		return fmt.Errorf("invalid BLS sig of Babylon raw checkpoint at epoch %d: %w", m.GetCurrentEpoch(), err)
	}
	// check whether the checkpoint from Babylon has the same LastCommitHash of the BTC checkpoint
	if !bbnCkpt.Ckpt.LastCommitHash.Equal(*btcCkpt.LastCommitHash) {
		return errors.Wrapf(types.ErrInconsistentLastCommitHash, fmt.Sprintf("Babylon checkpoint's LastCommitHash %s, BTC checkpoint's LastCommitHash %s",
			bbnCkpt.Ckpt.LastCommitHash.String(), btcCkpt.LastCommitHash))
	}
	return nil
}

func (m *Monitor) addCheckpointToCheckList(ckpt *types.CheckpointRecord) {
	record := types.NewCheckpointRecord(ckpt.RawCheckpoint, ckpt.FirstSeenBtcHeight)
	m.checkpointChecklist.Add(record)
}

func (m *Monitor) UpdateEpochInfo(epoch uint64) error {
	ei, err := m.BBNQuerier.QueryInfoForNextEpoch(epoch)
	if err != nil {
		return fmt.Errorf("failed to query information of the epoch %d: %w", epoch, err)
	}
	m.curEpoch = ei

	return nil
}

func (m *Monitor) checkHeaderConsistency(header *wire.BlockHeader) error {
	btcHeaderHash := header.BlockHash()

	contains, err := m.BBNQuerier.ContainsBTCHeader(&btcHeaderHash)
	if err != nil {
		return err
	}
	if !contains {
		return fmt.Errorf("BTC header %x does not exist on Babylon BTC light client", btcHeaderHash)
	}

	return nil
}

func GetSortedValSet(valSet checkpointingtypes.ValidatorWithBlsKeySet) checkpointingtypes.ValidatorWithBlsKeySet {
	sort.Slice(valSet.ValSet, func(i, j int) bool {
		addri, err := sdk.ValAddressFromBech32(valSet.ValSet[i].ValidatorAddress)
		if err != nil {
			panic(fmt.Errorf("failed to parse validator address %v: %w", valSet.ValSet[i].ValidatorAddress, err))
		}
		addrj, err := sdk.ValAddressFromBech32(valSet.ValSet[j].ValidatorAddress)
		if err != nil {
			panic(fmt.Errorf("failed to parse validator address %v: %w", valSet.ValSet[j].ValidatorAddress, err))
		}

		return sdk.BigEndianToUint64(addri) < sdk.BigEndianToUint64(addrj)
	})

	return valSet
}

// Stop signals all vigilante goroutines to shut down.
func (m *Monitor) Stop() {
	close(m.quit)
	m.BTCScanner.Stop()
}