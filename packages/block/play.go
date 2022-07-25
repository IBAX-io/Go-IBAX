/*----------------------------------------------------------------
- Copyright (c) IBAX. All rights reserved.
- See LICENSE in the project root for license information.
---------------------------------------------------------------*/

package block

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/IBAX-io/go-ibax/packages/common/random"
	"github.com/IBAX-io/go-ibax/packages/conf"
	"github.com/IBAX-io/go-ibax/packages/conf/syspar"
	"github.com/IBAX-io/go-ibax/packages/consts"
	"github.com/IBAX-io/go-ibax/packages/notificator"
	"github.com/IBAX-io/go-ibax/packages/pbgo"
	"github.com/IBAX-io/go-ibax/packages/script"
	"github.com/IBAX-io/go-ibax/packages/service/node"
	"github.com/IBAX-io/go-ibax/packages/storage/sqldb"
	"github.com/IBAX-io/go-ibax/packages/transaction"
	"github.com/IBAX-io/go-ibax/packages/types"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

// PlaySafe is inserting block safely
func (b *Block) PlaySafe() error {
	logger := b.GetLogger()
	dbTx, err := sqldb.StartTransaction()
	if err != nil {
		logger.WithFields(log.Fields{"type": consts.DBError, "error": err}).Error("starting db transaction")
		return err
	}

	inputTx := b.Transactions[:]
	err = b.ProcessTxs(dbTx)
	if err != nil {
		dbTx.Rollback()
		if b.GenBlock && len(b.TxFullData) == 0 {
			var banK = make(map[string]struct{}, len(inputTx))
			for i := 0; i < len(inputTx); i++ {
				var t, k = inputTx[i], strconv.FormatInt(inputTx[i].KeyID(), 10)
				if _, ok := banK[k]; !ok && t.IsSmartContract() {
					transaction.BadTxForBan(t.KeyID())
					banK[k] = struct{}{}
					continue
				}
				if err := transaction.MarkTransactionBad(t.Hash(), err.Error()); err != nil {
					return err
				}
			}
		}
		return err
	}

	if b.GenBlock && len(b.TxFullData) == 0 {
		dbTx.Commit()
		return ErrEmptyBlock
	}

	if err := b.InsertIntoBlockchain(dbTx); err != nil {
		dbTx.Rollback()
		return err
	}
	err = dbTx.Commit()
	if err != nil {
		return err
	}
	for _, q := range b.Notifications {
		q.Send()
	}
	return nil
}

func (b *Block) ProcessTxs(dbTx *sqldb.DbTransaction) (err error) {
	afters := &types.AfterTxs{
		Rts: make([]*types.RollbackTx, 0),
		Txs: make([]*types.AfterTx, 0),
	}
	logger := b.GetLogger()
	txsMap := make(map[int][]*transaction.Transaction)
	for k, txs := range b.ClassifyTxsMap {
		var transactions = make([]*transaction.Transaction, 0)
		for i := 0; i < len(txs); i++ {
			txBytes := txs[i]
			t, err := transaction.UnmarshallTransaction(bytes.NewBuffer(txBytes))
			if err != nil {
				if t != nil && t.Hash() != nil {
					_ = transaction.MarkTransactionBad(t.Hash(), err.Error())
				}
				continue
			}
			transactions = append(transactions, t)
		}
		txsMap[k] = transactions
	}
	limits := transaction.NewLimits(b.limitMode())
	rand := random.NewRand(b.Header.Timestamp)
	processedTx := make([][]byte, 0, len(b.Transactions))
	var genBErr error
	defer func() {
		if b.IsGenesis() || b.GenBlock {
			b.AfterTxs = afters
		}
		if b.GenBlock {
			b.TxFullData = processedTx
		}
		if genBErr != nil {
			err = genBErr
		}
		if errA := b.AfterPlayTxs(dbTx); errA != nil {
			if err == nil {
				err = errA
			} else if err != nil {
				err = fmt.Errorf("%v; %w", err, errA)
			}
			return
		}
	}()
	if !b.GenBlock && !b.IsGenesis() && conf.Config.BlockSyncMethod.Method == types.BlockSyncMethod_SQLDML.String() {
		if b.SysUpdate {
			if err := syspar.SysUpdate(dbTx); err != nil {
				return fmt.Errorf("updating syspar: %w", err)
			}
		}
		return nil
	}

	var keyIds []int64
	for indexTx := 0; indexTx < len(b.Transactions); indexTx++ {
		t := b.Transactions[indexTx]
		keyIds = append(keyIds, t.KeyID())
	}
	outputs, err := sqldb.GetTxOutputs(dbTx, keyIds)
	if err != nil {
		return err
	}
	b.OutputsMap = make(map[int64][]sqldb.SpentInfo)
	sqldb.PutAllOutputsMap(outputs, b.OutputsMap)

	var wg sync.WaitGroup

	// StopNetworkTxType
	if len(txsMap[types.StopNetworkTxType]) > 0 {
		transactions := txsMap[types.StopNetworkTxType]
		err := b.serialExecuteTxs(dbTx, logger, rand, limits, afters, &processedTx, transactions, lock, genBErr)
		delete(txsMap, types.StopNetworkTxType)
		if err != nil {
			return err
		}
	}

	// FirstBlockTxType
	if b.IsGenesis() {
		transactions := make([]*transaction.Transaction, 0)
		for _, tx := range b.Transactions {
			t, err := transaction.UnmarshallTransaction(bytes.NewBuffer(tx.FullData))
			if err != nil {
				if t != nil && t.Hash() != nil {
					transaction.MarkTransactionBad(t.Hash(), err.Error())
				}
				return fmt.Errorf("parse transaction error(%s)", err)
			}
			transactions = append(transactions, t)
		}
		err := b.serialExecuteTxs(dbTx, logger, rand, limits, afters, &processedTx, transactions, lock, genBErr)
		transactions = make([]*transaction.Transaction, 0)
		if err != nil {
			return err
		}
	}

	// DelayTxType
	if len(txsMap[types.DelayTxType]) > 0 {
		transactions := txsMap[types.DelayTxType]
		err := b.serialExecuteTxs(dbTx, logger, rand, limits, afters, &processedTx, transactions, lock, genBErr)
		delete(txsMap, types.DelayTxType)
		if err != nil {
			return err
		}
	}

	// TransferSelf
	if len(txsMap[types.TransferSelfTxType]) > 0 {
		transactions := txsMap[types.TransferSelfTxType]

		walletAddress := make(map[int64]int64)
		groupTransferSelfTxs(transactions, walletAddress)
		for g, transactions := range transferSelfTxsGroupMap {
			wg.Add(1)
			go func(_dbTx *sqldb.DbTransaction, _g string, _transactions []*transaction.Transaction, _afters *types.AfterTxs, _processedTx *[][]byte, _utxoTxsGroupMap map[string][]*transaction.Transaction, _lock *sync.RWMutex) {
				defer wg.Done()
				err := b.serialExecuteTxs(_dbTx, logger, rand, limits, _afters, _processedTx, _transactions, _lock, genBErr)
				if err != nil {
					return
				}
			}(dbTx, g, transactions, afters, &processedTx, transferSelfTxsGroupMap, lock)
		}
		wg.Wait()
		transferSelfTxsGroupMap = make(map[string][]*transaction.Transaction, 0)
		transferSelfGroupTxsList = make([]*transaction.Transaction, 0)
		transferSelfGroupSerial = 1
		delete(txsMap, types.TransferSelfTxType)
	}

	//Utxo && Smart contract
	if len(txsMap[types.UtxoTxType]) > 0 || len(txsMap[types.SmartContractTxType]) > 0 {
		transactions := txsMap[types.UtxoTxType]
		// utxo group
		walletAddress := make(map[int64]int64)
		groupUtxoTxs(transactions, walletAddress)
		if len(txsMap[types.SmartContractTxType]) > 0 {
			utxoTxsGroupMap[strconv.Itoa(0)] = txsMap[types.SmartContractTxType]
		}
		for g, transactions := range utxoTxsGroupMap {
			wg.Add(1)
			go func(_dbTx *sqldb.DbTransaction, _g string, _transactions []*transaction.Transaction, _afters *types.AfterTxs, _processedTx *[][]byte, _utxoTxsGroupMap map[string][]*transaction.Transaction, _lock *sync.RWMutex) {
				defer wg.Done()
				err := b.serialExecuteTxs(_dbTx, logger, rand, limits, _afters, _processedTx, _transactions, _lock, genBErr)
				if err != nil {
					return
				}
			}(dbTx, g, transactions, afters, &processedTx, utxoTxsGroupMap, lock)
		}
		wg.Wait()
		utxoTxsGroupMap = make(map[string][]*transaction.Transaction, 0)
		utxoGroupTxsList = make([]*transaction.Transaction, 0)
		utxoGroupSerial = 1
		delete(txsMap, types.UtxoTxType)
		delete(txsMap, types.SmartContractTxType)
	}

	return nil
}

func (b *Block) serialExecuteTxs(dbTx *sqldb.DbTransaction, logger *log.Entry, rand *random.Rand, limits *transaction.Limits, afters *types.AfterTxs, processedTx *[][]byte, txs []*transaction.Transaction, _lock *sync.RWMutex, genBErr error) error {
	_lock.Lock()
	defer _lock.Unlock()

	for curTx := 0; curTx < len(txs); curTx++ {
		t := txs[curTx]
		err := dbTx.Savepoint(consts.SetSavePointMarkBlock(hex.EncodeToString(t.Hash())))
		if err != nil {
			logger.WithFields(log.Fields{"type": consts.DBError, "error": err, "tx_hash": t.Hash()}).Error("using savepoint")
			return err
		}
		err = t.WithOption(notificator.NewQueue(), b.GenBlock, b.Header, b.PrevHeader, dbTx, rand.BytesSeed(t.Hash()), limits, consts.SetSavePointMarkBlock(hex.EncodeToString(t.Hash())), b.OutputsMap)
		if err != nil {
			return err
		}
		err = t.Play()
		if err != nil {
			if err == transaction.ErrNetworkStopping {
				// Set the node in a pause state
				node.PauseNodeActivity(node.PauseTypeStopingNetwork)
				return err
			}
			errRoll := t.DbTransaction.RollbackSavepoint(consts.SetSavePointMarkBlock(hex.EncodeToString(t.Hash())))
			if errRoll != nil {
				return fmt.Errorf("%v; %w", err, errRoll)
			}
			if b.GenBlock {
				if errors.Cause(err) == transaction.ErrLimitStop {
					if curTx == 0 {
						return err
					}
					break
				}
				if strings.Contains(err.Error(), script.ErrVMTimeLimit.Error()) {
					err = script.ErrVMTimeLimit
				}
			}
			if t.IsSmartContract() {
				transaction.BadTxForBan(t.KeyID())
			}
			_ = transaction.MarkTransactionBad(t.Hash(), err.Error())
			if t.SysUpdate {
				if err := syspar.SysUpdate(t.DbTransaction); err != nil {
					return fmt.Errorf("updating syspar: %w", err)
				}
				t.SysUpdate = false
			}
			if b.GenBlock {
				genBErr = err
				continue
			}
			return err
		}

		if t.SysUpdate {
			b.SysUpdate = true
			t.SysUpdate = false
		}

		if t.Notifications.Size() > 0 {
			b.Notifications = append(b.Notifications, t.Notifications)
		}

		var (
			after    = &types.AfterTx{}
			eco      int64
			contract string
			code     pbgo.TxInvokeStatusCode
		)
		if t.IsSmartContract() {
			eco = t.SmartContract().TxSmart.EcosystemID
			code = t.TxResult.Code
			if t.SmartContract().TxContract != nil {
				contract = t.SmartContract().TxContract.Name
			}
		}
		after.UsedTx = t.Hash()
		after.Lts = &types.LogTransaction{
			Block:        t.BlockHeader.BlockId,
			Hash:         t.Hash(),
			TxData:       t.FullData,
			Timestamp:    t.Timestamp(),
			Address:      t.KeyID(),
			EcosystemId:  eco,
			ContractName: contract,
			InvokeStatus: code,
		}
		after.UpdTxStatus = t.TxResult
		afters.Txs = append(afters.Txs, after)
		afters.Rts = append(afters.Rts, t.RollBackTx...)
		afters.TxBinLogSql = append(afters.TxBinLogSql, t.DbTransaction.BinLogSql...)
		*processedTx = append(*processedTx, t.FullData)

		sqldb.UpdateTxInputs(t.Hash(), t.TxInputs, b.OutputsMap)
		sqldb.InsertTxOutputs(t.Hash(), t.TxOutputs, b.OutputsMap)
	}

	return nil
}

var (
	utxoTxsGroupMap         = make(map[string][]*transaction.Transaction)
	utxoGroupTxsList        = make([]*transaction.Transaction, 0)
	utxoGroupSerial  uint16 = 1
	lock                    = &sync.RWMutex{}
)

func groupUtxoTxs(txs []*transaction.Transaction, walletAddress map[int64]int64) map[string][]*transaction.Transaction {
	if len(txs) == 0 {
		return utxoTxsGroupMap
	}
	crrentGroupTxsSize := len(utxoGroupTxsList)
	size := len(txs)
	for i := 0; i < size; i++ {
		if len(walletAddress) == 0 {
			walletAddress[txs[i].KeyID()] = txs[i].KeyID()
			walletAddress[txs[i].SmartContract().TxSmart.UTXO.ToID] = txs[i].SmartContract().TxSmart.UTXO.ToID

			utxoGroupTxsList = append(utxoGroupTxsList, txs[i])
			txs = txs[1:]
			size = len(txs)
			i--
			continue
		}
		if walletAddress[txs[i].KeyID()] != 0 || walletAddress[txs[i].SmartContract().TxSmart.UTXO.ToID] != 0 {
			walletAddress[txs[i].KeyID()] = txs[i].KeyID()
			walletAddress[txs[i].SmartContract().TxSmart.UTXO.ToID] = txs[i].SmartContract().TxSmart.UTXO.ToID

			utxoGroupTxsList = append(utxoGroupTxsList, txs[i])
			txs = append(txs[:i], txs[i+1:]...)
			size = len(txs)
			i--
		}
	}

	if crrentGroupTxsSize < len(utxoGroupTxsList) {
		if len(txs) == 0 {
			utxoTxsGroupMap[strconv.Itoa(int(utxoGroupSerial))] = utxoGroupTxsList
			return utxoTxsGroupMap
		}
		return groupUtxoTxs(txs, walletAddress)
	}

	if len(utxoGroupTxsList) > 0 {
		tempUtxoGroupTxsList := utxoGroupTxsList
		utxoTxsGroupMap[strconv.Itoa(int(utxoGroupSerial))] = tempUtxoGroupTxsList
		utxoGroupSerial++
		utxoGroupTxsList = make([]*transaction.Transaction, 0)
		walletAddress = make(map[int64]int64)
	}

	return groupUtxoTxs(txs, walletAddress)
}

var (
	transferSelfTxsGroupMap         = make(map[string][]*transaction.Transaction)
	transferSelfGroupTxsList        = make([]*transaction.Transaction, 0)
	transferSelfGroupSerial  uint16 = 1
)

func groupTransferSelfTxs(txs []*transaction.Transaction, walletAddress map[int64]int64) map[string][]*transaction.Transaction {
	if len(txs) == 0 {
		return transferSelfTxsGroupMap
	}
	crrentGroupTxsSize := len(transferSelfGroupTxsList)
	size := len(txs)
	for i := 0; i < size; i++ {
		if len(walletAddress) == 0 {
			walletAddress[txs[i].KeyID()] = txs[i].KeyID()

			transferSelfGroupTxsList = append(transferSelfGroupTxsList, txs[i])
			txs = txs[1:]
			size = len(txs)
			i--
			continue
		}
		if walletAddress[txs[i].KeyID()] != 0 {
			walletAddress[txs[i].KeyID()] = txs[i].KeyID()

			transferSelfGroupTxsList = append(transferSelfGroupTxsList, txs[i])
			txs = append(txs[:i], txs[i+1:]...)
			size = len(txs)
			i--
		}
	}

	if crrentGroupTxsSize < len(transferSelfGroupTxsList) {
		if len(txs) == 0 {
			transferSelfTxsGroupMap[strconv.Itoa(int(transferSelfGroupSerial))] = transferSelfGroupTxsList
			return transferSelfTxsGroupMap
		}
		return groupTransferSelfTxs(txs, walletAddress)
	}

	if len(transferSelfGroupTxsList) > 0 {
		tempTransferSelfGroupTxsList := transferSelfGroupTxsList
		transferSelfTxsGroupMap[strconv.Itoa(int(transferSelfGroupSerial))] = tempTransferSelfGroupTxsList
		transferSelfGroupSerial++
		transferSelfGroupTxsList = make([]*transaction.Transaction, 0)
		walletAddress = make(map[int64]int64)
	}

	return groupTransferSelfTxs(txs, walletAddress)
}
