package core

import (
	"encoding/json"
	"github.com/pkg/errors"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/dgraph-io/badger"
	"github.com/go-pg/pg"
	"github.com/nats-io/stan.go"
	"github.com/noah-blockchain/coinExplorer-tools/helpers"
	"github.com/noah-blockchain/coinExplorer-tools/models"
	"github.com/noah-blockchain/noah-extender/internal/address"
	"github.com/noah-blockchain/noah-extender/internal/balance"
	"github.com/noah-blockchain/noah-extender/internal/block"
	"github.com/noah-blockchain/noah-extender/internal/coin"
	"github.com/noah-blockchain/noah-extender/internal/events"
	"github.com/noah-blockchain/noah-extender/internal/transaction"
	"github.com/noah-blockchain/noah-extender/internal/validator"
	"github.com/noah-blockchain/noah-node-go-api"
	"github.com/noah-blockchain/noah-node-go-api/responses"
	"github.com/sirupsen/logrus"
)

const (
	ChasingModDiff    = 2
	CoinWorkerTimeout = time.Minute
)

type Extender struct {
	env                 *models.ExtenderEnvironment
	nodeApi             *noah_node_go_api.NoahNodeApi
	blockService        *block.Service
	addressService      *address.Service
	blockRepository     *block.Repository
	validatorService    *validator.Service
	validatorRepository *validator.Repository
	transactionService  *transaction.Service
	eventService        *events.Service
	balanceService      *balance.Service
	coinService         *coin.Service
	chasingMode         bool
	currentNodeHeight   uint64
	logger              *logrus.Entry
	dbBadger            *badger.DB
	db                  *pg.DB
}

type dbLogger struct {
	logger *logrus.Entry
}

func (d dbLogger) BeforeQuery(q *pg.QueryEvent) {}

func (d dbLogger) AfterQuery(q *pg.QueryEvent) {
	d.logger.Info(q.FormattedQuery())
}

func NewExtender(env *models.ExtenderEnvironment, db *pg.DB, dbBadger *badger.DB, ns stan.Conn, nodeApi *noah_node_go_api.NoahNodeApi) *Extender {
	//Init Logger
	logger := logrus.New()
	logger.SetFormatter(&logrus.JSONFormatter{})
	logger.SetOutput(os.Stdout)
	logger.SetReportCaller(true)

	if env.Debug {
		logger.SetFormatter(&logrus.TextFormatter{
			DisableColors: false,
			FullTimestamp: true,
		})
	} else {
		logger.SetFormatter(&logrus.JSONFormatter{})
		logger.SetLevel(logrus.WarnLevel)
	}

	contextLogger := logger.WithFields(logrus.Fields{
		"version": "2.1.0",
		"app":     "Coin Explorer Extender",
	})

	//if env.Debug {
	//	db.AddQueryHook(dbLogger{logger: contextLogger})
	//}
	//api

	// Repositories
	blockRepository := block.NewRepository(db)
	validatorRepository := validator.NewRepository(db)
	transactionRepository := transaction.NewRepository(db)
	addressRepository := address.NewRepository(db)
	coinRepository := coin.NewRepository(db)
	eventsRepository := events.NewRepository(db)
	balanceRepository := balance.NewRepository(db)

	// Services
	balanceService := balance.NewService(env, balanceRepository, nodeApi, addressRepository, coinRepository, contextLogger)
	coinService := coin.NewService(env, nodeApi, coinRepository, addressRepository, contextLogger, dbBadger, ns)
	return &Extender{
		env:                 env,
		nodeApi:             nodeApi,
		blockService:        block.NewBlockService(blockRepository, validatorRepository),
		eventService:        events.NewService(env, eventsRepository, validatorRepository, addressRepository, coinRepository, coinService, balanceRepository, contextLogger),
		blockRepository:     blockRepository,
		validatorService:    validator.NewService(env, nodeApi, validatorRepository, addressRepository, coinRepository, contextLogger),
		transactionService:  transaction.NewService(env, transactionRepository, addressRepository, validatorRepository, coinRepository, coinService, contextLogger),
		addressService:      address.NewService(env, addressRepository, balanceService.GetAddressesChannel(), contextLogger),
		validatorRepository: validatorRepository,
		balanceService:      balanceService,
		coinService:         coinService,
		chasingMode:         true,
		currentNodeHeight:   0,
		logger:              contextLogger,
		dbBadger:            dbBadger,
		db:                  db,
	}
}

func (ext *Extender) Run() {
	//check connections to node
	_, err := ext.nodeApi.GetStatus()
	if err == nil {
		err = ext.blockRepository.DeleteLastBlockData()
	} else {
		ext.logger.Error(err)
	}
	helpers.HandleError(err)

	var height uint64

	// ----- Workers -----
	ext.runWorkers()

	lastExplorerBlock, _ := ext.blockRepository.GetLastFromDB()

	if lastExplorerBlock != nil {
		height = lastExplorerBlock.ID + 1
		ext.blockService.SetBlockCache(lastExplorerBlock)
	} else {
		height = 1
	}

	for {
		//start := time.Now()
		ext.findOutChasingMode(height)
		//Pulling block data
		blockResponse, err := ext.nodeApi.GetBlock(height)
		helpers.HandleError(err)
		if blockResponse.Error != nil {
			time.Sleep(2 * time.Second)
			continue
		}

		//Pulling events
		eventsResponse, err := ext.nodeApi.GetBlockEvents(height)
		if err != nil {
			ext.logger.Error(err)
		}
		helpers.HandleError(err)

		ext.handleAddressesFromResponses(blockResponse, eventsResponse)
		ext.handleBlockResponse(blockResponse)
		ext.handleCoinsFromTransactions(blockResponse.Result.Transactions)

		if height%uint64(ext.env.RewardAggregateEveryBlocksCount) == 0 {
			go ext.eventService.AggregateRewards(ext.env.RewardAggregateTimeInterval, height)
		}
		go ext.handleEventResponse(height, eventsResponse)

		height++

		//elapsed := time.Since(start)
		//ext.logger.Info("Processing time: ", elapsed)
	}
}

func (ext *Extender) runWorkers() {

	// Addresses
	for w := 1; w <= ext.env.WrkSaveAddressesCount; w++ {
		go ext.addressService.SaveAddressesWorker(ext.addressService.GetSaveAddressesJobChannel())
	}

	// Transactions
	for w := 1; w <= ext.env.WrkSaveTxsCount; w++ {
		go ext.transactionService.SaveTransactionsWorker(ext.transactionService.GetSaveTxJobChannel())
	}
	for w := 1; w <= ext.env.WrkSaveTxsOutputCount; w++ {
		go ext.transactionService.SaveTransactionsOutputWorker(ext.transactionService.GetSaveTxsOutputJobChannel())
	}
	for w := 1; w <= ext.env.WrkSaveInvTxsCount; w++ {
		go ext.transactionService.SaveInvalidTransactionsWorker(ext.transactionService.GetSaveInvalidTxsJobChannel())
	}
	go ext.transactionService.UpdateTxsIndexWorker()

	// Validators
	for w := 1; w <= ext.env.WrkSaveValidatorTxsCount; w++ {
		go ext.transactionService.SaveTxValidatorWorker(ext.transactionService.GetSaveTxValidatorJobChannel())
	}
	//обновляет награды валидаторов
	go ext.validatorService.UpdateValidatorsWorker(ext.validatorService.GetUpdateValidatorsJobChannel())

	//обновляет стейки валидаторов
	go ext.validatorService.UpdateStakesWorker(ext.validatorService.GetUpdateStakesJobChannel())

	// Events
	for w := 1; w <= ext.env.WrkSaveRewardsCount; w++ {
		go ext.eventService.SaveRewardsWorker(ext.eventService.GetSaveRewardsJobChannel())
	}
	for w := 1; w <= ext.env.WrkSaveSlashesCount; w++ {
		go ext.eventService.SaveSlashesWorker(ext.eventService.GetSaveSlashesJobChannel())
	}

	// Balances
	go ext.balanceService.Run()
	for w := 1; w <= ext.env.WrkGetBalancesFromNodeCount; w++ {
		go ext.balanceService.GetBalancesFromNodeWorker(ext.balanceService.GetBalancesFromNodeChannel(), ext.balanceService.GetUpdateBalancesJobChannel())
	}
	for w := 1; w <= ext.env.WrkUpdateBalanceCount; w++ {
		go ext.balanceService.UpdateBalancesWorker(ext.balanceService.GetUpdateBalancesJobChannel())
	}

	//Coins
	go ext.coinService.UpdateCoinsInfoFromTxsWorker(ext.coinService.GetUpdateCoinsFromTxsJobChannel())
	go ext.coinService.UpdateCoinsInfoFromCoinsMap(ext.coinService.GetUpdateCoinsFromCoinsMapJobChannel())
	go ext.coinWorker()
	go ext.validatorUptimeWorker()
}

func (ext *Extender) handleAddressesFromResponses(blockResponse *responses.BlockResponse, eventsResponse *responses.EventsResponse) {
	err := ext.addressService.HandleResponses(blockResponse, eventsResponse)
	if err != nil {
		ext.logger.Error(err)
	}
	helpers.HandleError(err)
}

func (ext *Extender) handleBlockResponse(response *responses.BlockResponse) {
	// Save validators if not exist
	validators, err := ext.validatorService.HandleBlockResponse(response)
	if err != nil {
		ext.logger.Error(err)
	}
	helpers.HandleError(err)

	// Save block
	err = ext.blockService.HandleBlockResponse(response)
	if err != nil {
		ext.logger.Error(err)
	}
	helpers.HandleError(err)

	ext.linkBlockValidator(*response)

	//first block don't have validators
	if response.Result.TxCount != "0" && len(validators) > 0 {
		ext.handleTransactions(response, validators)
	}

	height, err := strconv.ParseUint(response.Result.Height, 10, 64)
	if err != nil {
		ext.logger.Error(err)
	}
	helpers.HandleError(err)

	// No need to update candidate and stakes at the same time
	// Candidate will be updated in the next iteration
	if height%12 == 0 {
		ext.validatorService.GetUpdateStakesJobChannel() <- height
	} else if height > 1 {
		ext.validatorService.GetUpdateValidatorsJobChannel() <- height
	}
}

func (ext *Extender) handleCoinsFromTransactions(transactions []responses.Transaction) {
	if len(transactions) == 0 {
		return
	}

	coins, err := ext.coinService.ExtractCoinsFromTransactions(transactions)
	if err != nil {
		ext.logger.Error(err)
		helpers.HandleError(err)
	}

	if len(coins) == 0 {
		return
	}

	err = ext.coinService.CreateNewCoins(coins)
	if err != nil {
		ext.logger.Error(err)
		helpers.HandleError(err)
	}
}

func (ext *Extender) handleTransactions(response *responses.BlockResponse, validators []*models.Validator) {
	height, err := strconv.ParseUint(response.Result.Height, 10, 64)
	if err != nil {
		ext.logger.Error(err)
	}
	helpers.HandleError(err)
	chunksCount := int(math.Ceil(float64(len(response.Result.Transactions)) / float64(ext.env.TxChunkSize)))
	for i := 0; i < chunksCount; i++ {
		start := ext.env.TxChunkSize * i
		end := start + ext.env.TxChunkSize
		if end > len(response.Result.Transactions) {
			end = len(response.Result.Transactions)
		}
		ext.saveTransactions(height, response.Result.Time, response.Result.Transactions[start:end])
	}
}

func (ext *Extender) handleEventResponse(blockHeight uint64, response *responses.EventsResponse) {
	if len(response.Result.Events) > 0 {
		//Save events
		err := ext.eventService.HandleEventResponse(blockHeight, response)
		if err != nil {
			ext.logger.Error(err)
		}
		helpers.HandleError(err)
	}
}

func (ext *Extender) linkBlockValidator(response responses.BlockResponse) {
	if response.Result.Height == "1" {
		return
	}
	var links []*models.BlockValidator
	height, err := strconv.ParseUint(response.Result.Height, 10, 64)
	if err != nil {
		ext.logger.Error(err)
	}
	helpers.HandleError(err)
	for _, v := range response.Result.Validators {
		vId, err := ext.validatorRepository.FindIdByPk(helpers.RemovePrefix(v.PubKey))
		if err != nil {
			ext.logger.Error(err)
		}
		helpers.HandleError(err)
		link := models.BlockValidator{
			ValidatorID: vId,
			BlockID:     height,
			Signed:      *v.Signed,
		}
		links = append(links, &link)
	}
	err = ext.blockRepository.LinkWithValidators(links)
	if err != nil {
		ext.logger.Error(err)
	}
	helpers.HandleError(err)
}

func (ext *Extender) saveTransactions(blockHeight uint64, blockCreatedAt time.Time, transactions []responses.Transaction) {
	// Save transactions
	err := ext.transactionService.HandleTransactionsFromBlockResponse(blockHeight, blockCreatedAt, transactions)
	if err != nil {
		ext.logger.Error(err)
	}
	helpers.HandleError(err)
}

func (ext *Extender) getNodeLastBlockId() (uint64, error) {
	statusResponse, err := ext.nodeApi.GetStatus()
	if err != nil {
		ext.logger.Error(err)
		return 0, err
	}
	return strconv.ParseUint(statusResponse.Result.LatestBlockHeight, 10, 64)
}

func (ext *Extender) findOutChasingMode(height uint64) {
	var err error
	if ext.currentNodeHeight == 0 {
		ext.currentNodeHeight, err = ext.getNodeLastBlockId()
		if err != nil {
			ext.logger.Error(err)
		}
		helpers.HandleError(err)
	}
	isChasingMode := ext.currentNodeHeight-height > ChasingModDiff
	if ext.chasingMode && !isChasingMode {
		ext.currentNodeHeight, err = ext.getNodeLastBlockId()
		if err != nil {
			ext.logger.Error(err)
		}
		helpers.HandleError(err)
		ext.chasingMode = ext.currentNodeHeight-height > ChasingModDiff
	}
}

func (ext *Extender) coinWorker() {
	for {
		//err := ext.dbBadger.Update(func(txn *badger.Txn) error {
		//	opts := badger.DefaultIteratorOptions
		//	opts.PrefetchValues = false
		//	it := txn.NewIterator(opts)
		//	for it.Rewind(); it.Valid(); it.Next() {
		//		item := it.Item()
		//
		//		value, err := item.ValueCopy(nil)
		//		key := item.Key()
		//		ext.logger.Println("KEY", string(key), "TRX_ID", string(value))
		//
		//		trx, err := ext.transactionService.FindTransactionByHash(string(value))
		//		if err != nil {
		//			ext.logger.Error(err)
		//			continue
		//		}
		//
		//		if err = ext.coinService.UpdateCoinMetaInfo(string(key), trx.ID, trx.FromAddressID); err != nil {
		//			ext.logger.Error(err)
		//			continue
		//		}
		//
		//		if err := txn.Delete(key); err != nil {
		//			ext.logger.Error(err)
		//			continue
		//		}
		//	}
		//	it.Close()
		//	return nil
		//})
		//if err != nil {
		//	ext.logger.Error(err)
		//}
		ext.FixBrokenCoinMetaInfo()
		ext.logger.Println("Coin Worker. New attempt")
		time.Sleep(CoinWorkerTimeout)
	}
}

type CustomTransaction struct {
	TrxID       uint64
	OwnerAddrID uint64
	Symbol      string
}

func (ext *Extender) FixBrokenCoinMetaInfo() {
	//get all coins where trx_id==nil and symbol != NOAH
	coins, err := ext.coinService.SelectCoinsWithBrokenMeta()
	if err != nil || coins == nil {
		ext.logger.Println(err)
		return
	}

	//get all trx where type == 5
	transactions, err := ext.transactionService.SelectCoinsTransaction()
	if err != nil || transactions == nil {
		ext.logger.Println(err)
		return
	}

	//create array entity with field id, owner_addr_id, symbol from trx
	trxs := make([]CustomTransaction, len(*transactions))
	for i, trx := range *transactions {
		obj := models.CreateCoinTxData{}
		if err := json.Unmarshal(trx.Data, &obj); err != nil {
			continue
		}

		trxs[i] = CustomTransaction{
			TrxID:       trx.ID,
			OwnerAddrID: trx.FromAddressID,
			Symbol:      obj.Symbol,
		}
	}

	//find where coin.symbol == trx.symbol
	//update coin info
	for _, c := range *coins {
		for _, trx := range trxs {
			if c.Symbol != trx.Symbol {
				continue
			}

			if err := ext.coinService.UpdateCoinMetaInfo(c.Symbol, trx.TrxID, trx.OwnerAddrID); err != nil {
				ext.logger.Println(err)
			}
		}
	}
}

func (ext *Extender) validatorUptimeWorker() {
	for {
		ext.updateValidatorsUptime()
		time.Sleep(5 * time.Minute)
	}
}

func (ext *Extender) updateValidatorsUptime() {
	validators, err := ext.validatorRepository.GetActiveValidators()
	if err != nil || validators == nil {
		ext.logger.Error(errors.WithStack(err))
		return
	}

	_ = ext.validatorRepository.ResetAllUptimes()
	for _, v := range *validators {
		signedCount, err := ext.validatorRepository.GetFullSignedCountValidatorBlock(v.ID, v.CreatedAt)
		if err != nil {
			ext.logger.Error(errors.WithStack(err))
			continue
		}
		//ext.logger.Println("Signed count", signedCount)

		validatorBlocksHeight, err := ext.validatorRepository.GetCountBlockFromDate(v.CreatedAt)
		if err != nil {
			ext.logger.Error(errors.WithStack(err))
			continue
		}
		//ext.logger.Println("Validator Blocks Height", validatorBlocksHeight)

		var value float64
		if validatorBlocksHeight > 0 {
			value = float64(signedCount) / float64(validatorBlocksHeight)
		}

		var uptime = math.Min(value*100, 100.0)
		if err = ext.validatorRepository.UpdateValidatorUptime(v.ID, uptime); err != nil {
			ext.logger.Error(errors.WithStack(err))
			continue
		}
	}
}

func (ext *Extender) Close() {
	ext.dbBadger.Close()
	ext.db.Close()
}
