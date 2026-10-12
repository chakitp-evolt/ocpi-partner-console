package cpo

import (
	"strings"
	"testing"
	"time"
)

func energyTariff() map[string]any {
	return map[string]any{
		"id": "BKK-ENERGY", "currency": "THB",
		"elements": []any{map[string]any{"price_components": []any{map[string]any{"type": "ENERGY", "price": 7.5, "vat": 7.0, "step_size": 1}}}},
	}
}

func TestEnergyTariffPricesFromThePublishedRateNotAHardCodedOne(t *testing.T) {
	price := priceSession(energyTariff(), "BKK-ENERGY", 4, 1.5, time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC))
	if !price.Priced || price.ExclVAT != 30 {
		t.Fatalf("excl_vat = %v (priced=%v), want 4 kWh × 7.5", price.ExclVAT, price.Priced)
	}
	if price.InclVAT != 32.10 {
		t.Fatalf("incl_vat = %v, want 30 + 7%% VAT", price.InclVAT)
	}
	if len(price.Lines) != 1 || price.Lines[0].Dimension != "ENERGY" {
		t.Fatalf("lines = %#v, want only the ENERGY component the tariff declares", price.Lines)
	}
}

func TestTimeIsNotBilledWhenTheTariffHasNoTimeComponent(t *testing.T) {
	price := priceSession(energyTariff(), "BKK-ENERGY", 0, 10, time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC))
	if price.ExclVAT != 0 {
		t.Fatalf("excl_vat = %v, want 0 — an energy-only tariff must not charge for time", price.ExclVAT)
	}
}

func TestStepSizeRoundsEnergyUpOncePerSession(t *testing.T) {
	tariff := map[string]any{
		"elements": []any{map[string]any{"price_components": []any{map[string]any{"type": "ENERGY", "price": 10.0, "vat": 0.0, "step_size": 500}}}},
	}
	// step_size for ENERGY counts in Wh, so 1.2 kWh is billed as 1.5 kWh.
	price := priceSession(tariff, "T", 1.2, 0, time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC))
	if price.ExclVAT != 15 {
		t.Fatalf("excl_vat = %v, want 1.5 kWh × 10 after rounding 1.2 kWh up to the 500 Wh step", price.ExclVAT)
	}
}

func TestFlatAndEnergyComeFromDifferentElementsWhenRestrictionsSaySo(t *testing.T) {
	tariff := map[string]any{
		"elements": []any{
			map[string]any{
				"price_components": []any{map[string]any{"type": "ENERGY", "price": 12.0, "vat": 0.0, "step_size": 1}},
				"restrictions":     map[string]any{"start_time": "18:00", "end_time": "23:59"},
			},
			map[string]any{"price_components": []any{
				map[string]any{"type": "FLAT", "price": 10.0, "vat": 0.0, "step_size": 1},
				map[string]any{"type": "ENERGY", "price": 8.0, "vat": 0.0, "step_size": 1},
			}},
		},
	}
	evening := priceSession(tariff, "T", 2, 0, time.Date(2026, 9, 10, 19, 0, 0, 0, time.UTC))
	if evening.ExclVAT != 34 {
		t.Fatalf("evening excl_vat = %v, want the restricted 12/kWh plus the unrestricted 10 FLAT", evening.ExclVAT)
	}
	morning := priceSession(tariff, "T", 2, 0, time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC))
	if morning.ExclVAT != 26 {
		t.Fatalf("morning excl_vat = %v, want the fallback 8/kWh plus the 10 FLAT", morning.ExclVAT)
	}
}

func TestAnUnevaluatedRestrictionNeverBillsTheRate(t *testing.T) {
	tariff := map[string]any{
		"elements": []any{map[string]any{
			"price_components": []any{map[string]any{"type": "ENERGY", "price": 99.0, "vat": 0.0, "step_size": 1}},
			"restrictions":     map[string]any{"max_current": 32},
		}},
	}
	price := priceSession(tariff, "T", 5, 0, time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC))
	if price.ExclVAT != 0 {
		t.Fatalf("excl_vat = %v, want 0 — the mock cannot check max_current so it must not bill that rate", price.ExclVAT)
	}
}

func TestTariffIDIsOnlyUsedWhenThePartnerActuallyHoldsThatTariff(t *testing.T) {
	connector := map[string]any{"tariff_ids": []any{"GONE", "BKK-ENERGY"}}
	tariffs := map[string]*Record{"BKK-ENERGY": {Body: energyTariff()}}
	if id, note := tariffIDForConnector(connector, tariffs, time.Now()); id != "BKK-ENERGY" || note != "" {
		t.Fatalf("id, note = %q, %q — want the tariff that exists", id, note)
	}
	id, note := tariffIDForConnector(map[string]any{"tariff_ids": []any{"GONE"}}, tariffs, time.Now())
	if id != "" || note == "" {
		t.Fatalf("id, note = %q, %q — a dangling tariff_id must be reported, not billed", id, note)
	}
}

func TestSumDimensionAddsUpOneDimensionAcrossPeriods(t *testing.T) {
	periods := []any{
		chargingPeriod("t1", "T", map[string]float64{"ENERGY": 1.25, "TIME": 0.5}),
		chargingPeriod("t2", "T", map[string]float64{"ENERGY": 2, "TIME": 0.25}),
	}
	if got := sumDimension(periods, "TIME"); got != 0.75 {
		t.Fatalf("TIME = %v, want 0.75", got)
	}
	if got := sumDimension(periods, "ENERGY"); got != 3.25 {
		t.Fatalf("ENERGY = %v, want 3.25", got)
	}
}

func TestClockWindowWrapsPastMidnightAndEndsAtMidnight(t *testing.T) {
	cases := []struct {
		now, start, end string
		want            bool
	}{
		{"23:00", "22:00", "06:00", true},
		{"05:59", "22:00", "06:00", true},
		{"06:00", "22:00", "06:00", false},
		{"12:00", "22:00", "06:00", false},
		{"20:00", "18:00", "00:00", true},
		{"17:59", "18:00", "00:00", false},
		{"13:00", "13:00", "", true},
		{"19:45", "", "19:45", false},
	}
	for _, tc := range cases {
		if got := inClockWindow(tc.now, tc.start, tc.end); got != tc.want {
			t.Errorf("inClockWindow(%s, %s-%s) = %v, want %v", tc.now, tc.start, tc.end, got, tc.want)
		}
	}
}

func TestNightRateAppliesAcrossMidnight(t *testing.T) {
	tariff := map[string]any{"elements": []any{
		map[string]any{"price_components": []any{map[string]any{"type": "ENERGY", "price": 4.0, "step_size": 1}}, "restrictions": map[string]any{"start_time": "22:00", "end_time": "06:00"}},
		map[string]any{"price_components": []any{map[string]any{"type": "ENERGY", "price": 8.0, "step_size": 1}}},
	}}
	if got := priceSession(tariff, "T", 2, 0, time.Date(2026, 10, 5, 23, 0, 0, 0, time.UTC)).ExclVAT; got != 8 {
		t.Fatalf("23:00 excl_vat = %v, want the 22:00-06:00 night rate 2 × 4", got)
	}
	if got := priceSession(tariff, "T", 2, 0, time.Date(2026, 10, 5, 7, 0, 0, 0, time.UTC)).ExclVAT; got != 16 {
		t.Fatalf("07:00 excl_vat = %v, want the day rate 2 × 8", got)
	}
}

// OCPI 2.2.1 §10.4.4 step_size example: €5/h before 17:00, €7/h after, step 10 min; 6 + 22 minutes
// round to 30 in total, so 24 minutes are billed after 17:00.
func TestTimeRateChangesMidSessionAndRoundsTheTotalOnce(t *testing.T) {
	tariff := map[string]any{"elements": []any{
		map[string]any{"price_components": []any{map[string]any{"type": "TIME", "price": 7.0, "step_size": 600}}, "restrictions": map[string]any{"start_time": "17:00", "end_time": "00:00"}},
		map[string]any{"price_components": []any{map[string]any{"type": "TIME", "price": 5.0, "step_size": 600}}},
	}}
	start := time.Date(2026, 10, 5, 16, 54, 0, 0, time.UTC)
	price := priceSession(tariff, "T", 0, 28.0/60, start)
	if len(price.Lines) != 2 || price.Lines[0].ExclVAT != 0.5 || price.Lines[1].ExclVAT != 2.8 {
		t.Fatalf("want 6 min × 5/h = 0.50 and 24 min × 7/h = 2.80, got %+v", price.Lines)
	}
}

// OCPI 2.2.1 §10.4.4 step_size example: 0.20/kWh before 17:00, 0.27 after, step 500 Wh; 4.3 + 1.1 kWh
// round to 5.5 kWh in total, so 1.2 kWh are billed after 17:00.
func TestEnergyRateChangesMidSessionAndRoundsTheTotalOnce(t *testing.T) {
	tariff := map[string]any{"elements": []any{
		map[string]any{"price_components": []any{map[string]any{"type": "ENERGY", "price": 0.27, "step_size": 500}}, "restrictions": map[string]any{"start_time": "17:00"}},
		map[string]any{"price_components": []any{map[string]any{"type": "ENERGY", "price": 0.20, "step_size": 500}}},
	}}
	at := func(hour, minute int) time.Time { return time.Date(2026, 10, 5, hour, minute, 0, 0, time.UTC) }
	spans := []chargeSpan{{Start: at(16, 0), End: at(17, 0), KWh: 4.3}, {Start: at(17, 0), End: at(17, 30), KWh: 1.1}}
	price := priceSessionWithParking(tariff, "T", spans, at(16, 0), at(17, 30), 0)
	if len(price.Lines) != 2 || price.Lines[0].ExclVAT != 0.86 || price.Lines[1].ExclVAT != 0.32 {
		t.Fatalf("want 4.3 kWh × 0.20 = 0.86 and 1.2 kWh × 0.27 = 0.32, got %+v", price.Lines)
	}
}

func TestDurationAndEnergyTiersBecomeActiveDuringTheSession(t *testing.T) {
	start := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	byDuration := map[string]any{"elements": []any{
		map[string]any{"price_components": []any{map[string]any{"type": "TIME", "price": 10.0, "step_size": 60}}, "restrictions": map[string]any{"max_duration": 3600}},
		map[string]any{"price_components": []any{map[string]any{"type": "TIME", "price": 100.0, "step_size": 60}}},
	}}
	if got := priceSession(byDuration, "T", 0, 2, start).ExclVAT; got != 110 {
		t.Fatalf("2 h with 10/h for the first hour then 100/h: excl_vat = %v, want 110", got)
	}
	byEnergy := map[string]any{"elements": []any{
		map[string]any{"price_components": []any{map[string]any{"type": "ENERGY", "price": 5.0, "step_size": 1}}, "restrictions": map[string]any{"max_kwh": 10}},
		map[string]any{"price_components": []any{map[string]any{"type": "ENERGY", "price": 8.0, "step_size": 1}}},
	}}
	if got := priceSession(byEnergy, "T", 15, 1, start).ExclVAT; got != 90 {
		t.Fatalf("15 kWh with 5/kWh below 10 kWh then 8/kWh: excl_vat = %v, want 90", got)
	}
}

func TestTariffSelectionSkipsTariffsOfAnotherTypeOrOutsideTheirValidity(t *testing.T) {
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	tariffs := map[string]*Record{
		"ADHOC":   {Body: map[string]any{"id": "ADHOC", "type": "AD_HOC_PAYMENT"}},
		"CHEAP":   {Body: map[string]any{"id": "CHEAP", "type": "PROFILE_CHEAP"}},
		"EXPIRED": {Body: map[string]any{"id": "EXPIRED", "end_date_time": "2026-10-05T12:00:00Z"}},
		"FUTURE":  {Body: map[string]any{"id": "FUTURE", "start_date_time": "2026-10-06T00:00:00Z"}},
		"REGULAR": {Body: map[string]any{"id": "REGULAR", "type": "REGULAR", "start_date_time": "2026-10-01T00:00:00Z"}},
	}
	connector := map[string]any{"tariff_ids": []any{"ADHOC", "CHEAP", "EXPIRED", "FUTURE", "REGULAR"}}
	if id, note := tariffIDForConnector(connector, tariffs, at); id != "REGULAR" || note != "" {
		t.Fatalf("id, note = %q, %q — want the REGULAR tariff valid at 12:00", id, note)
	}
	if id, note := tariffIDForConnector(map[string]any{"tariff_ids": []any{"ADHOC", "EXPIRED"}}, tariffs, at); id != "" || note == "" {
		t.Fatalf("id, note = %q, %q — no applicable tariff must be reported, not billed", id, note)
	}
}

// OCPI 2.2.1 §11.3.1.1 examples: max_price 10.00/11.00 over a 0.50 start fee and 0.25/kWh, and min_price 0.50/0.55.
func TestMinAndMaxPriceBoundTheSessionTotal(t *testing.T) {
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	capped := map[string]any{
		"max_price": map[string]any{"excl_vat": 10.0, "incl_vat": 11.0},
		"elements": []any{map[string]any{"price_components": []any{
			map[string]any{"type": "FLAT", "price": 0.50, "vat": 20.0, "step_size": 1},
			map[string]any{"type": "ENERGY", "price": 0.25, "vat": 10.0, "step_size": 1},
		}}},
	}
	if price := priceSession(capped, "T", 50, 1, at); price.ExclVAT != 10 || price.InclVAT != 11 {
		t.Fatalf("50 kWh = %v / %v, want the 10.00 / 11.00 ceiling", price.ExclVAT, price.InclVAT)
	}
	if price := priceSession(capped, "T", 30, 1, at); price.ExclVAT != 8 || price.InclVAT != 8.85 {
		t.Fatalf("30 kWh = %v / %v, want 8.00 / 8.85 below the ceiling", price.ExclVAT, price.InclVAT)
	}
	floored := map[string]any{
		"min_price": map[string]any{"excl_vat": 0.50, "incl_vat": 0.55},
		"elements":  []any{map[string]any{"price_components": []any{map[string]any{"type": "ENERGY", "price": 0.25, "vat": 10.0, "step_size": 1}}}},
	}
	if price := priceSession(floored, "T", 1, 1, at); price.ExclVAT != 0.5 || price.InclVAT != 0.55 || !price.Priced {
		t.Fatalf("1 kWh = %+v, want the 0.50 / 0.55 minimum", price)
	}
}

// OCPI 2.2.1 §10.3.1: a new ChargingPeriod starts at 17:00 when energy changes price, and the energy
// before 17:00 is listed in the period that ends there.
func TestChargingPeriodsAreCutWhereATariffElementChanges(t *testing.T) {
	tariff := map[string]any{"elements": []any{
		map[string]any{"price_components": []any{map[string]any{"type": "ENERGY", "price": 0.27, "step_size": 1}}, "restrictions": map[string]any{"start_time": "17:00"}},
		map[string]any{"price_components": []any{map[string]any{"type": "ENERGY", "price": 0.20, "step_size": 1}}, "restrictions": map[string]any{"max_kwh": 10}},
		map[string]any{"price_components": []any{map[string]any{"type": "ENERGY", "price": 0.30, "step_size": 1}}},
	}}
	session := map[string]any{"start_date_time": "2026-10-05T16:00:00.000Z", "kwh": 18.0}
	periods := chargePeriods(session, tariff, "T", time.UTC, time.Date(2026, 10, 5, 17, 30, 0, 0, time.UTC))
	starts := []string{}
	for _, raw := range periods {
		starts = append(starts, fieldText(raw.(map[string]any), "start_date_time"))
	}
	// 10 kWh is reached at 16:50 (18 kWh evenly over 90 minutes), and 17:00 starts the evening rate.
	want := []string{"2026-10-05T16:00:00.000Z", "2026-10-05T16:50:00.000Z", "2026-10-05T17:00:00.000Z"}
	if strings.Join(starts, ",") != strings.Join(want, ",") {
		t.Fatalf("period starts = %v, want %v", starts, want)
	}
	if got := sumDimension(periods, "ENERGY"); got < 17.999999 || got > 18.000001 {
		t.Fatalf("periods carry %v kWh, want all 18", got)
	}
}
