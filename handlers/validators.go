package handlers

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	v1 "github.com/attestantio/go-eth2-client/api/v1"

	"github.com/ethpandaops/dora/services"
	"github.com/ethpandaops/dora/templates"
	"github.com/ethpandaops/dora/types/models"
	"github.com/ethpandaops/dora/utils"
	"github.com/sirupsen/logrus"
)

// Validators will return the main "validators" page using a go template
func Validators(w http.ResponseWriter, r *http.Request) {
	var validatorsTemplateFiles = append(layoutTemplateFiles,
		"validators/validators.html",
		"_svg/professor.html",
	)

	var pageTemplate = templates.GetTemplate(validatorsTemplateFiles...)
	data := InitPageData(w, r, "validators", "/validators", "Validators", validatorsTemplateFiles)

	urlArgs := r.URL.Query()
	var firstIdx uint64 = 0
	if urlArgs.Has("s") {
		firstIdx, _ = strconv.ParseUint(urlArgs.Get("s"), 10, 64)
	}
	var pageSize uint64 = 50
	if urlArgs.Has("c") {
		pageSize, _ = strconv.ParseUint(urlArgs.Get("c"), 10, 64)
	}
	if urlArgs.Has("json") && pageSize > 10000 {
		pageSize = 10000
	} else if !urlArgs.Has("json") && pageSize > 1000 {
		pageSize = 1000
	}

	var filterPubKey string
	var filterIndex string
	var filterName string
	var filterStatus string
	if urlArgs.Has("f") {
		if urlArgs.Has("f.pubkey") {
			filterPubKey = urlArgs.Get("f.pubkey")
		}
		if urlArgs.Has("f.index") {
			filterIndex = urlArgs.Get("f.index")
		}
		if urlArgs.Has("f.name") {
			filterName = urlArgs.Get("f.name")
		}
		if urlArgs.Has("f.status") {
			filterStatus = strings.Join(urlArgs["f.status"], ",")
		}
	}
	var sortOrder string
	if urlArgs.Has("o") {
		sortOrder = urlArgs.Get("o")
	}

	var pageError error
	pageError = services.GlobalCallRateLimiter.CheckCallLimit(r, 1)
	if pageError == nil {
		data.Data, pageError = getValidatorsPageData(firstIdx, pageSize, sortOrder, filterPubKey, filterIndex, filterName, filterStatus)
	}
	if pageError != nil {
		handlePageError(w, r, pageError)
		return
	}

	if urlArgs.Has("json") {
		w.Header().Set("Content-Type", "application/json")
		err := json.NewEncoder(w).Encode(data.Data)
		if err != nil {
			logrus.WithError(err).Error("error encoding index data")
			http.Error(w, "Internal server error", http.StatusServiceUnavailable)
		}
	}

	w.Header().Set("Content-Type", "text/html")
	if handleTemplateError(w, r, "validators.go", "Validators", "", pageTemplate.ExecuteTemplate(w, "layout", data)) != nil {
		return // an error has occurred and was processed
	}
}

func getValidatorsPageData(firstValIdx uint64, pageSize uint64, sortOrder string, filterPubKey string, filterIndex string, filterName string, filterStatus string) (*models.ValidatorsPageData, error) {
	pageData := &models.ValidatorsPageData{}
	pageCacheKey := fmt.Sprintf("validators:%v:%v:%v:%v:%v:%v:%v", firstValIdx, pageSize, sortOrder, filterPubKey, filterIndex, filterName, filterStatus)
	pageRes, pageErr := services.GlobalFrontendCache.ProcessCachedPage(pageCacheKey, true, pageData, func(pageCall *services.FrontendCacheProcessingPage) interface{} {
		pageData, cacheTimeout := buildValidatorsPageData(firstValIdx, pageSize, sortOrder, filterPubKey, filterIndex, filterName, filterStatus)
		pageCall.CacheTimeout = cacheTimeout
		return pageData
	})
	if pageErr == nil && pageRes != nil {
		resData, resOk := pageRes.(*models.ValidatorsPageData)
		if !resOk {
			return nil, ErrInvalidPageModel
		}
		pageData = resData
	}
	return pageData, pageErr
}

func buildValidatorsPageData(firstValIdx uint64, pageSize uint64, sortOrder string, filterPubKey string, filterIndex string, filterName string, filterStatus string) (*models.ValidatorsPageData, time.Duration) {
	logrus.Debugf("validators page called: %v:%v:%v:%v:%v:%v:%v", firstValIdx, pageSize, sortOrder, filterPubKey, filterIndex, filterName, filterStatus)
	pageData := &models.ValidatorsPageData{}
	cacheTime := 10 * time.Minute

	chainState := services.GlobalBeaconService.GetChainState()

	// get latest validator set
	var validatorSet []*v1.Validator
	validatorSetRsp := services.GlobalBeaconService.GetCachedValidatorSet()
	if validatorSetRsp == nil {
		cacheTime = 5 * time.Minute
		validatorSet = []*v1.Validator{}
	} else {
		validatorSet = validatorSetRsp
	}

	// get status options
	statusMap := map[v1.ValidatorState]uint64{}
	for _, val := range validatorSet {
		statusMap[val.Status]++
	}
	pageData.FilterStatusOpts = make([]models.ValidatorsPageDataStatusOption, 0)
	for status, count := range statusMap {
		pageData.FilterStatusOpts = append(pageData.FilterStatusOpts, models.ValidatorsPageDataStatusOption{
			Status: status.String(),
			Count:  count,
		})
	}
	sort.Slice(pageData.FilterStatusOpts, func(a, b int) bool {
		return strings.Compare(pageData.FilterStatusOpts[a].Status, pageData.FilterStatusOpts[b].Status) < 0
	})

	filterArgs := url.Values{}
	if filterPubKey != "" || filterIndex != "" || filterName != "" || filterStatus != "" {
		var filterPubKeyVal []byte
		var filterIndexVal uint64
		var filterStatusVal []string

		if filterPubKey != "" {
			filterArgs.Add("f.pubkey", filterPubKey)
			filterPubKeyVal, _ = hex.DecodeString(strings.Replace(filterPubKey, "0x", "", -1))
		}
		if filterIndex != "" {
			filterArgs.Add("f.index", filterIndex)
			filterIndexVal, _ = strconv.ParseUint(filterIndex, 10, 64)
		}
		if filterName != "" {
			filterArgs.Add("f.name", filterName)
		}
		if filterStatus != "" {
			filterArgs.Add("f.status", filterStatus)
			filterStatusVal = strings.Split(filterStatus, ",")
		}

		// apply filter
		filteredValidatorSet := make([]*v1.Validator, 0)
		for _, val := range validatorSet {
			if filterPubKey != "" && !bytes.Equal(filterPubKeyVal, val.Validator.PublicKey[:]) {
				continue
			}
			if filterIndex != "" && filterIndexVal != uint64(val.Index) {
				continue
			}
			if filterName != "" {
				valName := services.GlobalBeaconService.GetValidatorName(uint64(val.Index))
				if !strings.Contains(valName, filterName) {
					continue
				}
			}
			if filterStatus != "" && !utils.SliceContains(filterStatusVal, val.Status.String()) {
				continue
			}
			filteredValidatorSet = append(filteredValidatorSet, val)
		}
		validatorSet = filteredValidatorSet
	}
	pageData.FilterPubKey = filterPubKey
	pageData.FilterIndex = filterIndex
	pageData.FilterName = filterName
	pageData.FilterStatus = filterStatus

	// apply sort order
	validatorSetLen := len(validatorSet)
	if sortOrder == "" {
		sortOrder = "index"
	}

	sortedValidatorSet := make([]*v1.Validator, validatorSetLen)
	copy(sortedValidatorSet, validatorSet)

	switch sortOrder {
	case "index":
		sort.Slice(sortedValidatorSet, func(a, b int) bool {
			return sortedValidatorSet[a].Index < sortedValidatorSet[b].Index
		})
		pageData.IsDefaultSorting = true
	case "index-d":
		sort.Slice(sortedValidatorSet, func(a, b int) bool {
			return sortedValidatorSet[a].Index > sortedValidatorSet[b].Index
		})
	case "pubkey":
		sort.Slice(sortedValidatorSet, func(a, b int) bool {
			return bytes.Compare(sortedValidatorSet[a].Validator.PublicKey[:], sortedValidatorSet[b].Validator.PublicKey[:]) < 0
		})
	case "pubkey-d":
		sort.Slice(sortedValidatorSet, func(a, b int) bool {
			return bytes.Compare(sortedValidatorSet[a].Validator.PublicKey[:], sortedValidatorSet[b].Validator.PublicKey[:]) > 0
		})
	case "balance":
		sort.Slice(sortedValidatorSet, func(a, b int) bool {
			return sortedValidatorSet[a].Balance < sortedValidatorSet[b].Balance
		})
	case "balance-d":
		sort.Slice(sortedValidatorSet, func(a, b int) bool {
			return sortedValidatorSet[a].Balance > sortedValidatorSet[b].Balance
		})
	case "activation":
		sort.Slice(sortedValidatorSet, func(a, b int) bool {
			return sortedValidatorSet[a].Validator.ActivationEpoch < sortedValidatorSet[b].Validator.ActivationEpoch
		})
	case "activation-d":
		sort.Slice(sortedValidatorSet, func(a, b int) bool {
			return sortedValidatorSet[a].Validator.ActivationEpoch > sortedValidatorSet[b].Validator.ActivationEpoch
		})
	case "exit":
		sort.Slice(sortedValidatorSet, func(a, b int) bool {
			return sortedValidatorSet[a].Validator.ExitEpoch < sortedValidatorSet[b].Validator.ExitEpoch
		})
	case "exit-d":
		sort.Slice(sortedValidatorSet, func(a, b int) bool {
			return sortedValidatorSet[a].Validator.ExitEpoch > sortedValidatorSet[b].Validator.ExitEpoch
		})
	}
	validatorSet = sortedValidatorSet
	pageData.Sorting = sortOrder

	totalValidatorCount := uint64(validatorSetLen)
	if firstValIdx == 0 {
		pageData.IsDefaultPage = true
	} else if firstValIdx > totalValidatorCount {
		firstValIdx = totalValidatorCount
	}

	pagesBefore := firstValIdx / pageSize
	if (firstValIdx % pageSize) > 0 {
		pagesBefore++
	}
	pagesAfter := (totalValidatorCount - firstValIdx) / pageSize
	if ((totalValidatorCount - firstValIdx) % pageSize) > 0 {
		pagesAfter++
	}
	pageData.PageSize = pageSize
	pageData.TotalPages = pagesBefore + pagesAfter
	pageData.CurrentPageIndex = pagesBefore + 1
	pageData.CurrentPageValIdx = firstValIdx
	if pagesBefore > 0 {
		pageData.PrevPageIndex = pageData.CurrentPageIndex - 1
		pageData.PrevPageValIdx = pageData.CurrentPageValIdx - pageSize
	}
	if pagesAfter > 1 {
		pageData.NextPageIndex = pageData.CurrentPageIndex + 1
		pageData.NextPageValIdx = pageData.CurrentPageValIdx + pageSize
	}
	pageData.LastPageValIdx = totalValidatorCount - pageSize

	// load activity map
	activityMap, maxActivity := services.GlobalBeaconService.GetValidatorActivity(3, false)

	// get validators
	lastValIdx := firstValIdx + pageSize
	if lastValIdx >= totalValidatorCount {
		lastValIdx = totalValidatorCount
	}
	pageData.Validators = make([]*models.ValidatorsPageDataValidator, 0)

	for _, validator := range validatorSet[firstValIdx:lastValIdx] {
		validatorData := &models.ValidatorsPageDataValidator{
			Index:            uint64(validator.Index),
			Name:             services.GlobalBeaconService.GetValidatorName(uint64(validator.Index)),
			PublicKey:        validator.Validator.PublicKey[:],
			Balance:          uint64(validator.Balance),
			EffectiveBalance: uint64(validator.Validator.EffectiveBalance),
		}
		if strings.HasPrefix(validator.Status.String(), "pending") {
			validatorData.State = "Pending"
		} else if validator.Status == v1.ValidatorStateActiveOngoing {
			validatorData.State = "Active"
			validatorData.ShowUpcheck = true
		} else if validator.Status == v1.ValidatorStateActiveExiting {
			validatorData.State = "Exiting"
			validatorData.ShowUpcheck = true
		} else if validator.Status == v1.ValidatorStateActiveSlashed {
			validatorData.State = "Slashed"
			validatorData.ShowUpcheck = true
		} else if validator.Status == v1.ValidatorStateExitedUnslashed {
			validatorData.State = "Exited"
		} else if validator.Status == v1.ValidatorStateExitedSlashed {
			validatorData.State = "Slashed"
		} else {
			validatorData.State = validator.Status.String()
		}

		if validatorData.ShowUpcheck {
			validatorData.UpcheckActivity = activityMap[validator.Index]
			validatorData.UpcheckMaximum = uint8(maxActivity)
		}

		if validator.Validator.ActivationEpoch < 18446744073709551615 {
			validatorData.ShowActivation = true
			validatorData.ActivationEpoch = uint64(validator.Validator.ActivationEpoch)
			validatorData.ActivationTs = chainState.EpochToTime(validator.Validator.ActivationEpoch)
		}
		if validator.Validator.ExitEpoch < 18446744073709551615 {
			validatorData.ShowExit = true
			validatorData.ExitEpoch = uint64(validator.Validator.ExitEpoch)
			validatorData.ExitTs = chainState.EpochToTime(validator.Validator.ExitEpoch)
		}
		if validator.Validator.WithdrawalCredentials[0] == 0x01 {
			validatorData.ShowWithdrawAddress = true
			validatorData.WithdrawAddress = validator.Validator.WithdrawalCredentials[12:]
		}

		pageData.Validators = append(pageData.Validators, validatorData)
	}
	pageData.ValidatorCount = uint64(len(pageData.Validators))
	pageData.FirstValidator = firstValIdx
	pageData.LastValidator = lastValIdx
	pageData.FilteredPageLink = fmt.Sprintf("/validators?f&%v&c=%v", filterArgs.Encode(), pageData.PageSize)

	return pageData, cacheTime
}
