package hetzner

// hourlyEUR holds Hetzner Cloud hourly list prices in EUR for Germany and
// Finland, excluding VAT and the primary IPv4, in force for new servers since
// 2026-06-15 (docs.hetzner.com/general/infrastructure-and-availability/price-adjustment,
// read 2026-10-11). Hetzner rounds a server's usage up to the next hour and
// bills it until deletion.
var hourlyEUR = map[string]float64{
	"cx33":  0.0136,
	"cx53":  0.0473,
	"cpx22": 0.0312,
	"cpx32": 0.0569,
	"cpx42": 0.1114,
	"ccx33": 0.2219,
}

// HourlyPriceEUR returns the listed hourly price of a server type.
func HourlyPriceEUR(serverType string) (float64, bool) {
	price, ok := hourlyEUR[serverType]
	return price, ok
}
