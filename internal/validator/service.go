package validator

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"time"

	"github.com/noah-blockchain/coinExplorer-tools/helpers"
	"github.com/noah-blockchain/coinExplorer-tools/models"
	"github.com/noah-blockchain/noah-extender/internal/address"
	"github.com/noah-blockchain/noah-extender/internal/coin"
	"github.com/noah-blockchain/noah-extender/internal/utils"
	"github.com/noah-blockchain/noah-node-go-api"
	"github.com/noah-blockchain/noah-node-go-api/responses"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const (
	precision                 = 100
	calcUptimeValidatorBlocks = 192
)

type Service struct {
	env                 *models.ExtenderEnvironment
	nodeApi             *noah_node_go_api.NoahNodeApi
	Repository          *Repository
	addressRepository   *address.Repository
	coinRepository      *coin.Repository
	jobUpdateValidators chan uint64
	jobUpdateStakes     chan uint64
	logger              *logrus.Entry
}

func NewService(env *models.ExtenderEnvironment, nodeApi *noah_node_go_api.NoahNodeApi, Repository *Repository,
	addressRepository *address.Repository, coinRepository *coin.Repository, logger *logrus.Entry) *Service {
	return &Service{
		env:                 env,
		nodeApi:             nodeApi,
		Repository:          Repository,
		addressRepository:   addressRepository,
		coinRepository:      coinRepository,
		logger:              logger,
		jobUpdateValidators: make(chan uint64, 1),
		jobUpdateStakes:     make(chan uint64, 1),
	}
}

func (s *Service) GetUpdateValidatorsJobChannel() chan uint64 {
	return s.jobUpdateValidators
}

func (s *Service) GetUpdateStakesJobChannel() chan uint64 {
	return s.jobUpdateStakes
}

func (s *Service) UpdateValidatorsWorker(jobs <-chan uint64) {
	for height := range jobs {
		resp, err := s.nodeApi.GetCandidates(height, false)
		if err != nil {
			s.logger.Println("height=%d", height)
			s.logger.Error(errors.WithStack(err))
			continue
		}

		if resp.Error != nil {
			//s.logger.Errorf("UpdateValidatorsWorker error: message=%s and data=%s height=%d", resp.Error.Message, resp.Error.Data, height) // todo
			continue
		}

		if len(resp.Result) > 0 {
			var (
				validators   = make([]*models.Validator, len(resp.Result))
				addressesMap = make(map[string]struct{})
			)

			// Collect all PubKey's and addresses for save it before
			for i, vlr := range resp.Result {
				validators[i] = &models.Validator{PublicKey: helpers.RemovePrefix(vlr.PubKey)}
				addressesMap[helpers.RemovePrefixFromAddress(vlr.RewardAddress)] = struct{}{}
				addressesMap[helpers.RemovePrefixFromAddress(vlr.OwnerAddress)] = struct{}{}
			}

			err = s.Repository.SaveAllIfNotExist(validators)
			if err != nil {
				s.logger.Error(errors.WithStack(err))
			}

			err = s.addressRepository.SaveFromMapIfNotExists(addressesMap)
			if err != nil {
				s.logger.Error(errors.WithStack(err))
			}

			for i, validator := range resp.Result {
				updateAt := time.Now()
				status := validator.Status
				totalStake := validator.TotalStake

				id, err := s.Repository.FindIdByPkOrCreate(helpers.RemovePrefix(validator.PubKey))
				if err != nil {
					s.logger.Error(errors.WithStack(err))
					continue
				}
				commission, err := strconv.ParseUint(validator.Commission, 10, 64)
				if err != nil {
					s.logger.Error(errors.WithStack(err))
					continue
				}
				rewardAddressID, err := s.addressRepository.FindIdOrCreate(helpers.RemovePrefixFromAddress(validator.RewardAddress))
				if err != nil {
					s.logger.Error(errors.WithStack(err))
					continue
				}
				ownerAddressID, err := s.addressRepository.FindIdOrCreate(helpers.RemovePrefixFromAddress(validator.OwnerAddress))
				if err != nil {
					s.logger.Error(errors.WithStack(err))
					continue
				}
				validators[i] = &models.Validator{
					ID:              id,
					Status:          &status,
					TotalStake:      &totalStake,
					UpdatedAt:       &updateAt,
					Commission:      &commission,
					RewardAddressID: &rewardAddressID,
					OwnerAddressID:  &ownerAddressID,
				}
			}
			err = s.Repository.ResetAllStatuses()
			if err != nil {
				s.logger.Error(errors.WithStack(err))
			}
			err = s.Repository.UpdateAll(validators)
			if err != nil {
				s.logger.Error(errors.WithStack(err))
			}
		}
	}
}

func (s *Service) UpdateStakesWorker(jobs <-chan uint64) {
	for height := range jobs {
		resp, err := s.nodeApi.GetCandidates(height, true)
		if err != nil {
			s.logger.Error(errors.WithStack(err))
			continue
		}

		if resp.Error != nil {
			//s.logger.Errorf("UpdateStakesWorker error: message=%s and data=%s", resp.Error.Message, resp.Error.Data) // todo
			continue
		}

		var (
			stakes       []*models.Stake
			stakesInCoin = make(map[uint64]string)
			validatorIds = make([]uint64, len(resp.Result))
			validators   = make([]*models.Validator, len(resp.Result))
			addressesMap = make(map[string]struct{})
		)

		// Collect all PubKey's and addresses for save it before
		for i, vlr := range resp.Result {
			validators[i] = &models.Validator{PublicKey: helpers.RemovePrefix(vlr.PubKey)}
			addressesMap[helpers.RemovePrefixFromAddress(vlr.RewardAddress)] = struct{}{}
			addressesMap[helpers.RemovePrefixFromAddress(vlr.OwnerAddress)] = struct{}{}
			for _, stake := range vlr.Stakes {
				addressesMap[helpers.RemovePrefixFromAddress(stake.Owner)] = struct{}{}
			}
		}

		err = s.Repository.SaveAllIfNotExist(validators)
		if err != nil {
			s.logger.Error(errors.WithStack(err))
		}

		err = s.addressRepository.SaveFromMapIfNotExists(addressesMap)
		if err != nil {
			s.logger.Error(errors.WithStack(err))
		}

		for i, vlr := range resp.Result {
			id, err := s.Repository.FindIdByPkOrCreate(helpers.RemovePrefix(vlr.PubKey))
			if err != nil {
				s.logger.Error(errors.WithStack(err))
				continue
			}
			validatorIds[i] = id

			for _, stake := range vlr.Stakes {
				ownerAddressID, err := s.addressRepository.FindIdOrCreate(helpers.RemovePrefixFromAddress(stake.Owner))
				if err != nil {
					s.logger.Error(errors.WithStack(err))
					continue
				}
				coinID, err := s.coinRepository.FindIdBySymbol(stake.Coin)
				if err != nil {
					s.logger.Error(errors.WithStack(err))
					continue
				}
				stakes = append(stakes, &models.Stake{
					ValidatorID:    id,
					OwnerAddressID: ownerAddressID,
					CoinID:         coinID,
					Value:          stake.Value,
					NoahValue:      stake.NoahValue,
				})

				if stake.Coin == s.env.BaseCoin {
					continue
				}

				v, ok := stakesInCoin[coinID]
				if ok {
					valueInt := utils.ConvertStringToBigInt(v)
					stakeInt := utils.ConvertStringToBigInt(stake.Value)
					valueInt.Add(valueInt, stakeInt)
					stakesInCoin[coinID] = valueInt.String()
				} else {
					stakesInCoin[coinID] = stake.Value
				}
			}
		}

		chunksCount := int(math.Ceil(float64(len(stakes)) / float64(s.env.StakeChunkSize)))
		for i := 0; i < chunksCount; i++ {
			start := s.env.StakeChunkSize * i
			end := start + s.env.StakeChunkSize
			if end > len(stakes) {
				end = len(stakes)
			}
			err = s.Repository.SaveAllStakes(stakes[start:end])
			if err != nil {
				s.logger.Error(errors.WithStack(err))
				panic(err)
			}
		}

		stakesId := make([]uint64, len(stakes))
		for i, stake := range stakes {
			stakesId[i] = stake.ID
		}
		if err = s.Repository.DeleteStakesNotInListIds(stakesId); err != nil {
			s.logger.Error(errors.WithStack(err))
		}

		coinsId := make([]uint64, len(stakesInCoin))
		index := 0
		for k, v := range stakesInCoin { // compute how much was delegated in literally coin into validators
			coinsId[index] = k
			index++

			go func(coinID uint64, stake string) {
				fmt.Println(fmt.Sprintf("coinID = %d stake = %s", coinID, stake))
				currentCoin, err := s.coinRepository.FindCoinByID(coinID)
				if err != nil {
					s.logger.Error(errors.WithStack(err))
					return
				}

				stakeFloat, _ := utils.NewFloat(0, precision).SetString(stake)
				volumeFloat, _ := utils.NewFloat(0, precision).SetString(currentCoin.Volume)
				stakeFloat.Quo(stakeFloat, volumeFloat)
				stakeFloat.Mul(stakeFloat, big.NewFloat(100))
				delegated, _ := stakeFloat.Uint64()

				if err = s.coinRepository.UpdateCoinDelegation(coinID, utils.Min(delegated, 100)); err != nil {
					s.logger.Error(errors.WithStack(err))
					return
				}
			}(k, v)
		}

		if err = s.coinRepository.ResetCoinDelegationNotInListIds(coinsId); err != nil {
			s.logger.Error(errors.WithStack(err))
		}

		if height%calcUptimeValidatorBlocks == 0 { //update uptime
			for _, validatorID := range validatorIds {
				// calc count validators
				go func(validatorID uint64) {
					countDelegators, err := s.Repository.GetCountDelegators(validatorID)
					if err != nil {
						s.logger.Error(errors.WithStack(err))
						return
					}

					if err = s.Repository.UpdateCountDelegators(validatorID, countDelegators); err != nil {
						s.logger.Error(errors.WithStack(err))
						return
					}
				}(validatorID)
			}
		}
	}
}

//Get validators PK from response and store it to validators table if not exist
func (s *Service) HandleBlockResponse(response *responses.BlockResponse) ([]*models.Validator, error) {
	var validators []*models.Validator
	for _, v := range response.Result.Validators {
		validators = append(validators, &models.Validator{PublicKey: helpers.RemovePrefix(v.PubKey)})
	}
	err := s.Repository.SaveAllIfNotExist(validators)
	if err != nil {
		s.logger.Error(errors.WithStack(err))
		return nil, err
	}
	return validators, err
}

func (s *Service) HandleCandidateResponse(response *responses.CandidateResponse) (*models.Validator, []*models.Stake, error) {
	validator := new(models.Validator)
	validator.Status = &response.Result.Status
	validator.TotalStake = &response.Result.TotalStake
	commission, err := strconv.ParseUint(response.Result.Commission, 10, 64)
	if err != nil {
		s.logger.Error(errors.WithStack(err))
		return nil, nil, err
	}
	validator.Commission = &commission
	createdAtBlockID, err := strconv.ParseUint(response.Result.CreatedAtBlock, 10, 64)
	if err != nil {
		s.logger.Error(errors.WithStack(err))
		return nil, nil, err
	}
	validator.CreatedAtBlockID = &createdAtBlockID
	ownerAddressID, err := s.addressRepository.FindIdOrCreate(helpers.RemovePrefixFromAddress(response.Result.OwnerAddress))
	if err != nil {
		s.logger.Error(errors.WithStack(err))
		return nil, nil, err
	}
	validator.OwnerAddressID = &ownerAddressID
	rewardAddressID, err := s.addressRepository.FindIdOrCreate(helpers.RemovePrefixFromAddress(response.Result.RewardAddress))
	if err != nil {
		s.logger.Error(errors.WithStack(err))
		return nil, nil, err
	}
	validator.RewardAddressID = &rewardAddressID
	validator.PublicKey = helpers.RemovePrefix(response.Result.PubKey)
	validatorID, err := s.Repository.FindIdByPk(validator.PublicKey)
	if err != nil {
		s.logger.Error(errors.WithStack(err))
		return nil, nil, err
	}
	validator.ID = validatorID
	now := time.Now()
	validator.UpdatedAt = &now

	stakes, err := s.GetStakesFromCandidateResponse(response)
	if err != nil {
		s.logger.Error(errors.WithStack(err))
		return nil, nil, err
	}

	return validator, stakes, nil
}

func (s *Service) GetStakesFromCandidateResponse(response *responses.CandidateResponse) ([]*models.Stake, error) {
	var stakes []*models.Stake
	validatorID, err := s.Repository.FindIdByPk(helpers.RemovePrefix(response.Result.PubKey))
	if err != nil {
		s.logger.Error(errors.WithStack(err))
		return nil, err
	}
	for _, stake := range response.Result.Stakes {
		ownerAddressID, err := s.addressRepository.FindId(helpers.RemovePrefixFromAddress(stake.Owner))
		if err != nil {
			s.logger.Error(errors.WithStack(err))
			return nil, err
		}
		coinID, err := s.coinRepository.FindIdBySymbol(stake.Coin)
		if err != nil {
			s.logger.Error(errors.WithStack(err))
			return nil, err
		}
		stakes = append(stakes, &models.Stake{
			CoinID:         coinID,
			Value:          stake.Value,
			ValidatorID:    validatorID,
			NoahValue:      stake.NoahValue,
			OwnerAddressID: ownerAddressID,
		})
	}
	return stakes, nil
}
