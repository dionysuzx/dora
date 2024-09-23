package handlers

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethpandaops/dora/dbtypes"
	"github.com/ethpandaops/dora/services"
	"github.com/ethpandaops/dora/templates"
	"github.com/ethpandaops/dora/types/models"
	"github.com/sirupsen/logrus"
)

// ElWithdrawals will return the filtered "el_withdrawals" page using a go template
func ElWithdrawals(w http.ResponseWriter, r *http.Request) {
	var templateFiles = append(layoutTemplateFiles,
		"el_withdrawals/el_withdrawals.html",
		"_svg/professor.html",
	)

	var pageTemplate = templates.GetTemplate(templateFiles...)
	data := InitPageData(w, r, "validators", "/validators/el_withdrawals", "Withdrawal Requests", templateFiles)

	urlArgs := r.URL.Query()
	var pageSize uint64 = 50
	if urlArgs.Has("c") {
		pageSize, _ = strconv.ParseUint(urlArgs.Get("c"), 10, 64)
	}
	var pageIdx uint64 = 1
	if urlArgs.Has("p") {
		pageIdx, _ = strconv.ParseUint(urlArgs.Get("p"), 10, 64)
		if pageIdx < 1 {
			pageIdx = 1
		}
	}

	var minSlot uint64
	var maxSlot uint64
	var sourceAddr string
	var minIndex uint64
	var maxIndex uint64
	var vname string
	var withOrphaned uint64
	var withType uint64

	if urlArgs.Has("f") {
		if urlArgs.Has("f.mins") {
			minSlot, _ = strconv.ParseUint(urlArgs.Get("f.mins"), 10, 64)
		}
		if urlArgs.Has("f.maxs") {
			maxSlot, _ = strconv.ParseUint(urlArgs.Get("f.maxs"), 10, 64)
		}
		if urlArgs.Has("f.address") {
			sourceAddr = urlArgs.Get("f.address")
		}
		if urlArgs.Has("f.mini") {
			minIndex, _ = strconv.ParseUint(urlArgs.Get("f.mini"), 10, 64)
		}
		if urlArgs.Has("f.maxi") {
			maxIndex, _ = strconv.ParseUint(urlArgs.Get("f.maxi"), 10, 64)
		}
		if urlArgs.Has("f.vname") {
			vname = urlArgs.Get("f.vname")
		}
		if urlArgs.Has("f.orphaned") {
			withOrphaned, _ = strconv.ParseUint(urlArgs.Get("f.orphaned"), 10, 64)
		}
		if urlArgs.Has("f.type") {
			withType, _ = strconv.ParseUint(urlArgs.Get("f.type"), 10, 64)
		}
	} else {
		withOrphaned = 1
	}
	var pageError error
	pageError = services.GlobalCallRateLimiter.CheckCallLimit(r, 2)
	if pageError == nil {
		data.Data, pageError = getFilteredElWithdrawalsPageData(pageIdx, pageSize, minSlot, maxSlot, sourceAddr, minIndex, maxIndex, vname, uint8(withOrphaned), uint8(withType))
	}
	if pageError != nil {
		handlePageError(w, r, pageError)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	if handleTemplateError(w, r, "el_withdrawals.go", "ElWithdrawals", "", pageTemplate.ExecuteTemplate(w, "layout", data)) != nil {
		return // an error has occurred and was processed
	}
}

func getFilteredElWithdrawalsPageData(pageIdx uint64, pageSize uint64, minSlot uint64, maxSlot uint64, sourceAddr string, minIndex uint64, maxIndex uint64, vname string, withOrphaned uint8, withType uint8) (*models.ElWithdrawalsPageData, error) {
	pageData := &models.ElWithdrawalsPageData{}
	pageCacheKey := fmt.Sprintf("el_withdrawals:%v:%v:%v:%v:%v:%v:%v:%v:%v:%v", pageIdx, pageSize, minSlot, maxSlot, sourceAddr, minIndex, maxIndex, vname, withOrphaned, withType)
	pageRes, pageErr := services.GlobalFrontendCache.ProcessCachedPage(pageCacheKey, true, pageData, func(_ *services.FrontendCacheProcessingPage) interface{} {
		return buildFilteredElWithdrawalsPageData(pageIdx, pageSize, minSlot, maxSlot, sourceAddr, minIndex, maxIndex, vname, withOrphaned, withType)
	})
	if pageErr == nil && pageRes != nil {
		resData, resOk := pageRes.(*models.ElWithdrawalsPageData)
		if !resOk {
			return nil, ErrInvalidPageModel
		}
		pageData = resData
	}
	return pageData, pageErr
}

func buildFilteredElWithdrawalsPageData(pageIdx uint64, pageSize uint64, minSlot uint64, maxSlot uint64, sourceAddr string, minIndex uint64, maxIndex uint64, vname string, withOrphaned uint8, withType uint8) *models.ElWithdrawalsPageData {
	filterArgs := url.Values{}
	if minSlot != 0 {
		filterArgs.Add("f.mins", fmt.Sprintf("%v", minSlot))
	}
	if maxSlot != 0 {
		filterArgs.Add("f.maxs", fmt.Sprintf("%v", maxSlot))
	}
	if sourceAddr != "" {
		filterArgs.Add("f.address", sourceAddr)
	}
	if minIndex != 0 {
		filterArgs.Add("f.mini", fmt.Sprintf("%v", minIndex))
	}
	if maxIndex != 0 {
		filterArgs.Add("f.maxi", fmt.Sprintf("%v", maxIndex))
	}
	if vname != "" {
		filterArgs.Add("f.vname", vname)
	}
	if withOrphaned != 0 {
		filterArgs.Add("f.orphaned", fmt.Sprintf("%v", withOrphaned))
	}
	if withType != 0 {
		filterArgs.Add("f.type", fmt.Sprintf("%v", withType))
	}

	pageData := &models.ElWithdrawalsPageData{
		FilterAddress:       sourceAddr,
		FilterMinSlot:       minSlot,
		FilterMaxSlot:       maxSlot,
		FilterMinIndex:      minIndex,
		FilterMaxIndex:      maxIndex,
		FilterValidatorName: vname,
		FilterWithOrphaned:  withOrphaned,
		FilterWithType:      withType,
	}
	logrus.Debugf("el_withdrawals page called: %v:%v [%v,%v,%v,%v,%v]", pageIdx, pageSize, minSlot, maxSlot, minIndex, maxIndex, vname)
	if pageIdx == 1 {
		pageData.IsDefaultPage = true
	}

	if pageSize > 100 {
		pageSize = 100
	}
	pageData.PageSize = pageSize
	pageData.TotalPages = pageIdx
	pageData.CurrentPageIndex = pageIdx
	if pageIdx > 1 {
		pageData.PrevPageIndex = pageIdx - 1
	}

	// load voluntary exits
	withdrawalRequestFilter := &dbtypes.WithdrawalRequestFilter{
		MinSlot:       minSlot,
		MaxSlot:       maxSlot,
		SourceAddress: common.FromHex(sourceAddr),
		MinIndex:      minIndex,
		MaxIndex:      maxIndex,
		ValidatorName: vname,
		WithOrphaned:  withOrphaned,
	}

	switch withType {
	case 1: // withdrawals
		minAmount := uint64(1)
		withdrawalRequestFilter.MinAmount = &minAmount
	case 2: // exits
		maxAmount := uint64(0)
		withdrawalRequestFilter.MaxAmount = &maxAmount
	}

	dbElWithdrawals, totalRows := services.GlobalBeaconService.GetWithdrawalRequestsByFilter(withdrawalRequestFilter, pageIdx-1, uint32(pageSize))

	chainState := services.GlobalBeaconService.GetChainState()
	validatorSetRsp := services.GlobalBeaconService.GetCachedValidatorSet()

	for _, elWithdrawal := range dbElWithdrawals {
		elWithdrawalData := &models.ElWithdrawalsPageDataWithdrawal{
			SlotNumber: elWithdrawal.SlotNumber,
			SlotRoot:   elWithdrawal.SlotRoot,
			Time:       chainState.SlotToTime(phase0.Slot(elWithdrawal.SlotNumber)),
			Orphaned:   elWithdrawal.Orphaned,
			SourceAddr: elWithdrawal.SourceAddress,
			Amount:     elWithdrawal.Amount,
			PublicKey:  elWithdrawal.ValidatorPubkey,
		}

		if elWithdrawal.ValidatorIndex != nil {
			elWithdrawalData.ValidatorIndex = *elWithdrawal.ValidatorIndex
			elWithdrawalData.ValidatorName = services.GlobalBeaconService.GetValidatorName(*elWithdrawal.ValidatorIndex)

			if uint64(len(validatorSetRsp)) > elWithdrawalData.ValidatorIndex && validatorSetRsp[elWithdrawalData.ValidatorIndex] != nil {
				elWithdrawalData.ValidatorValid = true
			}
		}

		pageData.ElRequests = append(pageData.ElRequests, elWithdrawalData)
	}
	pageData.RequestCount = uint64(len(pageData.ElRequests))

	if pageData.RequestCount > 0 {
		pageData.FirstIndex = pageData.ElRequests[0].SlotNumber
		pageData.LastIndex = pageData.ElRequests[pageData.RequestCount-1].SlotNumber
	}

	pageData.TotalPages = totalRows / pageSize
	if totalRows%pageSize > 0 {
		pageData.TotalPages++
	}
	pageData.LastPageIndex = pageData.TotalPages
	if pageIdx < pageData.TotalPages {
		pageData.NextPageIndex = pageIdx + 1
	}

	pageData.FirstPageLink = fmt.Sprintf("/validators/el_withdrawals?f&%v&c=%v", filterArgs.Encode(), pageData.PageSize)
	pageData.PrevPageLink = fmt.Sprintf("/validators/el_withdrawals?f&%v&c=%v&p=%v", filterArgs.Encode(), pageData.PageSize, pageData.PrevPageIndex)
	pageData.NextPageLink = fmt.Sprintf("/validators/el_withdrawals?f&%v&c=%v&p=%v", filterArgs.Encode(), pageData.PageSize, pageData.NextPageIndex)
	pageData.LastPageLink = fmt.Sprintf("/validators/el_withdrawals?f&%v&c=%v&p=%v", filterArgs.Encode(), pageData.PageSize, pageData.LastPageIndex)

	return pageData
}
