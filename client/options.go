package client

type EligiblePiecesOption = func(*EligiblePiecesConfig)

type EligiblePiecesConfig struct {
	TenantID int16
	Limit    uint64
}

func WithEligiblePiecesTenantID(tenantID int16) EligiblePiecesOption {
	return func(opts *EligiblePiecesConfig) {
		opts.TenantID = tenantID
	}
}

func WithEligiblePiecesLimit(limit uint64) EligiblePiecesOption {
	return func(opts *EligiblePiecesConfig) {
		opts.Limit = limit
	}
}

type ReservePieceOption = func(*ReservePieceConfig)

type ReservePieceConfig struct {
	TenantID     int16
	TenantPolicy string
}

// WithReservePieceTenantID sets an optional tenant ID the piece should be
// reserved with.
func WithReservePieceTenantID(tenantID int16) ReservePieceOption {
	return func(opts *ReservePieceConfig) {
		opts.TenantID = tenantID
	}
}

// WithReservePieceTenantPolicy sets an optional tenant policy string to
// validate against when reserving the piece. Note: if the chosen tenant has a
// policy configured, this must match. i.e. this is mandatory for tenants that
// have a policy set.
func WithReservePieceTenantPolicy(tenantPolicy string) ReservePieceOption {
	return func(opts *ReservePieceConfig) {
		opts.TenantPolicy = tenantPolicy
	}
}
