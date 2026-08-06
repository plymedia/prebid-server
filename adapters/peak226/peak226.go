package peak226

import (
	"fmt"
	"net/http"
	"strings"
	"text/template"

	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v4/adapters"
	"github.com/prebid/prebid-server/v4/config"
	"github.com/prebid/prebid-server/v4/errortypes"
	"github.com/prebid/prebid-server/v4/macros"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
	"github.com/prebid/prebid-server/v4/util/jsonutil"
)

const (
	defaultRegion = "us"
	currencyUSD   = "USD"
)

type adapter struct {
	endpoint *template.Template
}

// Builder builds a new instance of the Peak226 adapter for the given bidder with the given config.
func Builder(bidderName openrtb_ext.BidderName, config config.Adapter, server config.Server) (adapters.Bidder, error) {
	endpointTemplate, err := template.New("endpointTemplate").Parse(config.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("unable to parse endpoint url template: %v", err)
	}

	bidder := &adapter{
		endpoint: endpointTemplate,
	}
	return bidder, nil
}

func (a *adapter) MakeRequests(request *openrtb2.BidRequest, reqInfo *adapters.ExtraRequestInfo) ([]*adapters.RequestData, []error) {
	var errs []error

	requestCopy := *request
	imps := make([]openrtb2.Imp, 0, len(requestCopy.Imp))

	var publisherID string
	region := defaultRegion
	haveRegion := false

	for _, imp := range requestCopy.Imp {
		var bidderExt adapters.ExtImpBidder
		if err := jsonutil.Unmarshal(imp.Ext, &bidderExt); err != nil {
			errs = append(errs, &errortypes.BadInput{
				Message: fmt.Sprintf("imp #%s: %s", imp.ID, err.Error()),
			})
			continue
		}

		var peak226Ext openrtb_ext.ImpExtPeak226
		if err := jsonutil.Unmarshal(bidderExt.Bidder, &peak226Ext); err != nil {
			errs = append(errs, &errortypes.BadInput{
				Message: fmt.Sprintf("imp #%s: %s", imp.ID, err.Error()),
			})
			continue
		}

		imp.TagID = peak226Ext.PlacementID
		imp.Ext = nil

		if imp.BidFloor > 0 && imp.BidFloorCur != "" && !strings.EqualFold(imp.BidFloorCur, currencyUSD) {
			convertedValue, err := reqInfo.ConvertCurrency(imp.BidFloor, imp.BidFloorCur, currencyUSD)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			imp.BidFloor = convertedValue
			imp.BidFloorCur = currencyUSD
		}

		if publisherID == "" && peak226Ext.PublisherID != "" {
			publisherID = peak226Ext.PublisherID
		}
		if !haveRegion && peak226Ext.Region != "" {
			region = peak226Ext.Region
			haveRegion = true
		}

		imps = append(imps, imp)
	}

	if len(imps) == 0 {
		return nil, errs
	}

	requestCopy.Imp = imps
	setPublisherID(&requestCopy, publisherID)

	endpoint, err := macros.ResolveMacros(a.endpoint, macros.EndpointTemplateParams{Region: region})
	if err != nil {
		errs = append(errs, err)
		return nil, errs
	}

	requestJSON, err := jsonutil.Marshal(&requestCopy)
	if err != nil {
		errs = append(errs, err)
		return nil, errs
	}

	headers := http.Header{}
	headers.Add("Content-Type", "application/json;charset=utf-8")
	headers.Add("Accept", "application/json")

	requestData := &adapters.RequestData{
		Method:  http.MethodPost,
		Uri:     endpoint,
		Body:    requestJSON,
		Headers: headers,
		ImpIDs:  openrtb_ext.GetImpIDs(requestCopy.Imp),
	}

	return []*adapters.RequestData{requestData}, errs
}

// setPublisherID mirrors the Prebid.js adapter's behavior of writing the publisherId
// param onto app.publisher.id when the request is an app request, or site.publisher.id otherwise.
func setPublisherID(request *openrtb2.BidRequest, publisherID string) {
	if publisherID == "" {
		return
	}

	if request.App != nil {
		appCopy := *request.App
		appCopy.Publisher = clonePublisher(appCopy.Publisher, publisherID)
		request.App = &appCopy
		return
	}

	var siteCopy openrtb2.Site
	if request.Site != nil {
		siteCopy = *request.Site
	}
	siteCopy.Publisher = clonePublisher(siteCopy.Publisher, publisherID)
	request.Site = &siteCopy
}

func clonePublisher(publisher *openrtb2.Publisher, id string) *openrtb2.Publisher {
	if publisher == nil {
		return &openrtb2.Publisher{ID: id}
	}
	publisherCopy := *publisher
	publisherCopy.ID = id
	return &publisherCopy
}

func (a *adapter) MakeBids(request *openrtb2.BidRequest, requestData *adapters.RequestData, response *adapters.ResponseData) (*adapters.BidderResponse, []error) {
	if adapters.IsResponseStatusCodeNoContent(response) {
		return nil, nil
	}

	if err := adapters.CheckResponseStatusCodeForErrors(response); err != nil {
		return nil, []error{err}
	}

	var bidResp openrtb2.BidResponse
	if err := jsonutil.Unmarshal(response.Body, &bidResp); err != nil {
		return nil, []error{&errortypes.BadServerResponse{
			Message: fmt.Sprintf("bad server response: %s", err.Error()),
		}}
	}

	if len(bidResp.SeatBid) == 0 {
		return adapters.NewBidderResponse(), nil
	}

	var errs []error
	bidderResponse := adapters.NewBidderResponseWithBidsCapacity(len(bidResp.SeatBid[0].Bid))
	if bidResp.Cur != "" {
		bidderResponse.Currency = bidResp.Cur
	}

	for _, seatBid := range bidResp.SeatBid {
		for i := range seatBid.Bid {
			bid := seatBid.Bid[i]

			bidType, err := getMediaTypeForBid(bid)
			if err != nil {
				errs = append(errs, err)
				continue
			}

			bidderResponse.Bids = append(bidderResponse.Bids, &adapters.TypedBid{
				Bid:     &bid,
				BidType: bidType,
			})
		}
	}

	return bidderResponse, errs
}

func getMediaTypeForBid(bid openrtb2.Bid) (openrtb_ext.BidType, error) {
	switch bid.MType {
	case openrtb2.MarkupBanner:
		return openrtb_ext.BidTypeBanner, nil
	case openrtb2.MarkupVideo:
		return openrtb_ext.BidTypeVideo, nil
	case openrtb2.MarkupNative:
		return openrtb_ext.BidTypeNative, nil
	}

	return "", &errortypes.BadServerResponse{
		Message: fmt.Sprintf("unrecognized bid type for impression %s", bid.ImpID),
	}
}
