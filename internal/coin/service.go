package coin

import (
	"strconv"
	"time"

	"github.com/dgraph-io/badger"
	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes"
	"github.com/nats-io/stan.go"
	coin_extender "github.com/noah-blockchain/coinExplorer-tools"
	"github.com/noah-blockchain/coinExplorer-tools/helpers"
	"github.com/noah-blockchain/coinExplorer-tools/models"
	node_models "github.com/noah-blockchain/noah-explorer-tools/models"
	"github.com/noah-blockchain/noah-extender/internal/address"
	"github.com/noah-blockchain/noah-node-go-api"
	"github.com/noah-blockchain/noah-node-go-api/responses"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type Service struct {
	env                   *models.ExtenderEnvironment
	nodeApi               *noah_node_go_api.NoahNodeApi
	repository            *Repository
	addressRepository     *address.Repository
	logger                *logrus.Entry
	jobUpdateCoins        chan []*models.Transaction
	jobUpdateCoinsFromMap chan map[string]struct{}
	dbBadger              *badger.DB
	ns                    stan.Conn
}

func NewService(env *models.ExtenderEnvironment, nodeApi *noah_node_go_api.NoahNodeApi, repository *Repository,
	addressRepository *address.Repository, logger *logrus.Entry, dbBadger *badger.DB, ns stan.Conn) *Service {

	return &Service{
		env:                   env,
		nodeApi:               nodeApi,
		repository:            repository,
		addressRepository:     addressRepository,
		logger:                logger,
		jobUpdateCoins:        make(chan []*models.Transaction, 1),
		jobUpdateCoinsFromMap: make(chan map[string]struct{}, 1),
		dbBadger:              dbBadger,
		ns:                    ns,
	}
}

type CreateCoinData struct {
	Name           string `json:"name"`
	Symbol         string `json:"symbol"`
	InitialAmount  string `json:"initial_amount"`
	InitialReserve string `json:"initial_reserve"`
	Crr            string `json:"crr"`
}

func (s *Service) GetUpdateCoinsFromTxsJobChannel() chan []*models.Transaction {
	return s.jobUpdateCoins
}

func (s *Service) GetUpdateCoinsFromCoinsMapJobChannel() chan map[string]struct{} {
	return s.jobUpdateCoinsFromMap
}

func AppendIfMissing(slice []*models.Coin, c *models.Coin) []*models.Coin {
	for _, ele := range slice {
		if ele.Symbol == c.Symbol {
			return slice
		}
	}
	return append(slice, c)
}

func (s Service) ExtractCoinsFromTransactions(transactions []responses.Transaction) ([]*models.Coin, error) {
	var coins []*models.Coin
	for _, tx := range transactions {
		if tx.Type != models.TxTypeCreateCoin {
			continue
		}

		if tx.Log != nil { // protection. Coin maybe not created in blockchain
			s.logger.Error(*tx.Log)
			continue
		}

		coin, err := s.ExtractFromTx(tx)
		if err != nil {
			s.logger.Error(err)
			return nil, err
		}
		coins = AppendIfMissing(coins, coin)
	}
	return coins, nil
}

func (s *Service) ExtractFromTx(tx responses.Transaction) (*models.Coin, error) {
	if tx.Data == nil {
		s.logger.Warn("empty transaction data")
		return nil, errors.New("no data for creating a coin")
	}

	if tx.Log != nil {
		return nil, errors.New(*tx.Log)
	}

	txData := tx.IData.(node_models.CreateCoinTxData)

	crr, err := strconv.ParseUint(txData.ConstantReserveRatio, 10, 64)
	if err != nil {
		s.logger.Error(err)
		return nil, err
	}

	coin := &models.Coin{
		Crr:                 crr,
		Volume:              txData.InitialAmount,
		ReserveBalance:      txData.InitialReserve,
		Name:                txData.Name,
		Symbol:              txData.Symbol,
		DeletedAt:           nil,
		Price:               GetTokenPrice(txData.InitialAmount, txData.InitialReserve, crr),
		StartVolume:         txData.InitialAmount,
		StartReserveBalance: txData.InitialReserve,
	}
	coin.Capitalization = GetCapitalization(coin.Volume, coin.Price)
	coin.StartPrice = coin.Price

	if coin.Symbol != s.env.BaseCoin {
		go func(symbol, hash string) {
			err = s.dbBadger.Update(func(txn *badger.Txn) error {
				return txn.Set([]byte(symbol), []byte(hash))
			})
			s.logger.Error(err)
		}(coin.Symbol, helpers.RemovePrefix(tx.Hash))

		go s.eventCoinMessage(&coin_extender.Coin{
			Symbol:         coin.Symbol,
			Price:          coin.Price,
			Capitalization: coin.Capitalization,
			ReserveBalance: coin.ReserveBalance,
			Volume:         coin.Volume,
			CreatedAt:      ptypes.TimestampNow(),
		})
	}

	return coin, nil
}

func (s *Service) CreateNewCoins(coins []*models.Coin) error {
	err := s.repository.SaveAllIfNotExist(coins)
	if err != nil {
		s.logger.Error(err)
	}
	return err
}

func (s *Service) UpdateCoinsInfoFromTxsWorker(jobs <-chan []*models.Transaction) {
	for transactions := range jobs {
		coinsMap := make(map[string]struct{})
		// Find coins in transaction for update
		for _, tx := range transactions {
			symbol, err := s.repository.FindSymbolById(tx.GasCoinID)
			if err != nil {
				s.logger.Error(err)
				continue
			}
			coinsMap[symbol] = struct{}{}
			switch tx.Type {
			case models.TxTypeSellCoin:
				coinsMap[tx.IData.(node_models.SellCoinTxData).CoinToBuy] = struct{}{}
				coinsMap[tx.IData.(node_models.SellCoinTxData).CoinToSell] = struct{}{}
			case models.TxTypeBuyCoin:
				coinsMap[tx.IData.(node_models.BuyCoinTxData).CoinToBuy] = struct{}{}
				coinsMap[tx.IData.(node_models.BuyCoinTxData).CoinToSell] = struct{}{}
			case models.TxTypeSellAllCoin:
				coinsMap[tx.IData.(node_models.SellAllCoinTxData).CoinToBuy] = struct{}{}
				coinsMap[tx.IData.(node_models.SellAllCoinTxData).CoinToSell] = struct{}{}
			}
		}
		s.GetUpdateCoinsFromCoinsMapJobChannel() <- coinsMap
	}
}

func (s Service) UpdateCoinsInfoFromCoinsMap(job <-chan map[string]struct{}) {
	for coinsMap := range job {
		delete(coinsMap, s.env.BaseCoin)
		if len(coinsMap) > 0 {
			coinsForUpdate := make([]string, len(coinsMap))
			i := 0
			for symbol := range coinsMap {
				coinsForUpdate[i] = symbol
				i++
			}
			err := s.UpdateCoinsInfo(coinsForUpdate)
			if err != nil {
				s.logger.Error(err)
			}
		}
	}
}

func (s *Service) UpdateCoinsInfo(symbols []string) error {
	var coins []*models.Coin
	for _, symbol := range symbols {
		if symbol == s.env.BaseCoin {
			continue
		}
		coin, err := s.GetCoinFromNode(symbol)
		if err != nil {
			s.logger.Error(err)
			continue
		}
		coins = append(coins, coin)
	}
	if len(coins) > 0 {
		return s.repository.SaveAllIfNotExist(coins)
	}
	return nil
}

func (s *Service) GetCoinFromNode(symbol string) (*models.Coin, error) {
	coinResp, err := s.nodeApi.GetCoinInfo(symbol)
	if err != nil {
		s.logger.Error(err)
		return nil, err
	}
	coin := new(models.Coin)
	id, err := s.repository.FindIdBySymbol(symbol)
	if err != nil {
		s.logger.Error(err)
		return nil, err
	}
	coin.ID = id
	if coinResp.Error != nil {
		return nil, errors.New(coinResp.Error.Message)
	}
	crr, err := strconv.ParseUint(coinResp.Result.Crr, 10, 64)
	if err != nil {
		s.logger.Error(err)
		return nil, err
	}
	coin.Name = coinResp.Result.Name
	coin.Symbol = coinResp.Result.Symbol
	coin.Crr = crr
	coin.ReserveBalance = coinResp.Result.ReserveBalance
	coin.Volume = coinResp.Result.Volume
	coin.DeletedAt = nil
	coin.UpdatedAt = time.Now()
	coin.Price = GetTokenPrice(coinResp.Result.Volume, coinResp.Result.ReserveBalance, crr)
	coin.Capitalization = GetCapitalization(coin.Volume, coin.Price)

	if coin.Symbol != s.env.BaseCoin {
		go s.eventCoinMessage(&coin_extender.Coin{
			Symbol:         coin.Symbol,
			Price:          coin.Price,
			Capitalization: coin.Capitalization,
			ReserveBalance: coin.ReserveBalance,
			Volume:         coin.Volume,
			CreatedAt:      ptypes.TimestampNow(),
		})
	}
	return coin, nil
}

func (s *Service) UpdateCoinMetaInfo(symbol string, trxId, ownerAddrId uint64) error {
	if err := s.repository.UpdateCoinMetaInfo(symbol, trxId, ownerAddrId); err != nil {
		return err
	}
	return nil
}

func (s *Service) SelectCoinsWithBrokenMeta() (*[]models.Coin, error) {
	coins, err := s.repository.SelectCoinsWithBrokenMeta()
	if err != nil || coins == nil {
		return nil, err
	}
	return coins, nil
}

func (s *Service) eventCoinMessage(coin *coin_extender.Coin) {
	data, _ := proto.Marshal(coin)

	err := s.ns.Publish(helpers.CoinCreatedSubject, data)
	if err != nil {
		s.logger.Error(errors.WithStack(err))
	}
}
