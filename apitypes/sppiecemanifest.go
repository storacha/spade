package apitypes

// ResponsePieceManifest is the response payload returned by the .../sp/piece_manifest endpoint
type ResponsePieceManifestFR58 struct {
	AggPCidV2 string    `json:"frc58_aggregate"`
	Segments  []Segment `json:"piece_list"`
}
type Segment struct {
	PCidV2  string   `json:"pcid_v2"`
	Sources []Source `json:"sources"`
}
type Source struct {
	URL     string              `json:"url"`
	Headers map[string][]string `json:"headers,omitempty"`
}

func (ResponsePieceManifestFR58) is() isResponsePayload { return isResponsePayload{} }
