package market

// The SDK exposes these metadata fields as strings. Its documented lowercase
// values and the API's enum-shaped values describe the same contracts. Normalize
// only known aliases at the API boundary, keeping saved domain values stable.
// Empty, unspecified and unrecognized values remain unsupported.
func futureSettlementType(raw string) string {
	switch raw {
	case "physical_delivery", "DELIVERY_TYPE_PHYSICAL_DELIVERY":
		return "physical_delivery"
	case "cash_settlement", "DELIVERY_TYPE_CASH_SETTLEMENT":
		return "cash_settlement"
	default:
		return ""
	}
}

func futureAssetType(raw string) string {
	switch raw {
	case "security", "TYPE_SECURITY":
		return "security"
	case "index", "TYPE_INDEX":
		return "index"
	case "currency", "TYPE_CURRENCY":
		return "currency"
	case "commodity", "TYPE_COMMODITY":
		return "commodity"
	default:
		return ""
	}
}
