package core

// ValidClientThroughput checks a measured client download rate in bits/s.
func ValidClientThroughput(bps float64) bool {
	return bps > 0 && bps <= 10_000_000_000
}

// ClientDeliveryEstimator keeps last-mile measurements separate from provider
// reads. Its budget is advisory and zero until there is fresh corroboration.
type ClientDeliveryEstimator interface {
	ObserveClientThroughput(viewerID int64, caps ClientCaps, bps float64)
	ClientDeliveryBudgetKbps(viewerID int64, caps ClientCaps) int
}

type AutoSelectionReason string

const (
	AutoConnectionEstimate AutoSelectionReason = "connection_estimate"
	AutoSourceDelivery     AutoSelectionReason = "source_delivery"
	AutoResumeSource       AutoSelectionReason = "resume_source"
	AutoSourceSelection    AutoSelectionReason = "source_selection"
)
