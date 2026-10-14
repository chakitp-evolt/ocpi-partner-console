package cpo

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/ocpi"
)

// Pricing follows OCPI 2.2.1 §11.4 (Tariff object). Two rules drive everything here:
//
//   - Price Components are looked up per dimension: the first TariffElement in the list that both
//     carries a Price Component for that dimension and whose restrictions match is the one that
//     applies. Different dimensions may therefore resolve to different elements.
//   - step_size is taken into account once per session — once for ENERGY and once for TIME and
//     PARKING_TIME combined — not once per Charging Period.
//
// A stopped charge may remain plugged in. PARKING_TIME is measured separately from charging TIME.

// knownRestrictions are the TariffRestrictions fields this mock evaluates. An element carrying any
// other restriction is treated as not matching, so the mock never bills a rate whose condition it
// cannot actually check.
var knownRestrictions = map[string]bool{
	"start_time": true, "end_time": true, "day_of_week": true,
	"min_kwh": true, "max_kwh": true, "min_duration": true, "max_duration": true,
}

type priceLine struct {
	Dimension string  `json:"dimension"`
	Price     float64 `json:"price"`
	VAT       float64 `json:"vat"`
	StepSize  float64 `json:"step_size"`
	Volume    float64 `json:"volume"`
	ExclVAT   float64 `json:"excl_vat"`
	InclVAT   float64 `json:"incl_vat"`
}

type sessionPrice struct {
	TariffID string      `json:"tariff_id"`
	Priced   bool        `json:"priced"`
	ExclVAT  float64     `json:"excl_vat"`
	InclVAT  float64     `json:"incl_vat"`
	Lines    []priceLine `json:"lines"`
	Note     string      `json:"note"`
}

func (p sessionPrice) totalCost() map[string]any {
	return map[string]any{"excl_vat": p.ExclVAT, "incl_vat": p.InclVAT}
}

// tariffIDForConnector returns the connector's tariff that applies to a session starting at `at`: the
// first tariff_id the partner holds a Tariff object for, inside that Tariff's start_date_time and
// end_date_time window, whose type fits a regular session. An omitted type is valid for all sessions;
// AD_HOC_PAYMENT and PROFILE_* tariffs only apply to ad-hoc payment or a charging preference, neither of
// which this mock runs (OCPI 2.2.1 §8.3.3 tariff_ids, §11.3.1 type/start_date_time/end_date_time).
func tariffIDForConnector(connector map[string]any, tariffs map[string]*Record, at time.Time) (string, string) {
	ids, _ := connector["tariff_ids"].([]any)
	var skipped []string
	for _, raw := range ids {
		id := strings.TrimSpace(fmt.Sprint(raw))
		if id == "" {
			continue
		}
		record := tariffs[id]
		if record == nil {
			skipped = append(skipped, id+" (the partner holds no such Tariff object)")
			continue
		}
		if reason := tariffNotApplicable(record.Body, at); reason != "" {
			skipped = append(skipped, id+" ("+reason+")")
			continue
		}
		return id, ""
	}
	if len(skipped) > 0 {
		return "", "no connector tariff applies: " + strings.Join(skipped, ", ")
	}
	return "", "connector has no tariff_ids"
}

// tariffNotApplicable says why a Tariff cannot price a regular session starting at `at`, or "" if it can.
func tariffNotApplicable(tariff map[string]any, at time.Time) string {
	switch kind := strings.ToUpper(fieldText(tariff, "type")); kind {
	case "", "REGULAR":
	default:
		return "type " + kind + " is not for a regular session"
	}
	if start, err := parseOCPITime(fieldText(tariff, "start_date_time")); err == nil && at.Before(start) {
		return "not active before " + fieldText(tariff, "start_date_time")
	}
	if end, err := parseOCPITime(fieldText(tariff, "end_date_time")); err == nil && !at.Before(end) {
		return "no longer valid after " + fieldText(tariff, "end_date_time")
	}
	return ""
}

// applyPriceLimits enforces Tariff.min_price and max_price (OCPI 2.2.1 §11.3.1): each bound holds for the
// excl. VAT and the incl. VAT total separately, and a bound without incl_vat limits the excl. VAT total only.
// The per-dimension lines keep what the components priced; only the session total is bounded.
func applyPriceLimits(tariff map[string]any, price sessionPrice) (sessionPrice, bool) {
	limited := false
	bound := func(field string, below bool) {
		limit, ok := tariff[field].(map[string]any)
		if !ok {
			return
		}
		apply := func(current *float64, key string) {
			value, set := limit[key]
			if !set || value == nil {
				return
			}
			if amount := roundMoney(number(value)); (below && *current < amount) || (!below && *current > amount) {
				*current = amount
				limited = true
				price.Note = field + " applied"
			}
		}
		apply(&price.ExclVAT, "excl_vat")
		apply(&price.InclVAT, "incl_vat")
	}
	bound("min_price", true)
	bound("max_price", false)
	return price, limited
}

// reachedMaxPrice reports whether a Session total sits on the Tariff's max_price on either side it bounds.
func reachedMaxPrice(tariff map[string]any, cost map[string]any) bool {
	limit, ok := tariff["max_price"].(map[string]any)
	if !ok || cost == nil {
		return false
	}
	for _, key := range []string{"excl_vat", "incl_vat"} {
		if value, set := limit[key]; set && value != nil && roundMoney(number(cost[key])) >= roundMoney(number(value)) {
			return true
		}
	}
	return false
}

// chargeSpan is one stretch of charging and the energy delivered across it, taken as evenly spread.
type chargeSpan struct {
	Start, End time.Time
	KWh        float64
}

type periodEntry struct {
	start, end time.Time
	kwh        float64
	charging   bool
}

// periodEntries reads a Session's ChargingPeriods in time order. A period ends when the next one starts
// (OCPI 2.2.1 §10.4.6); the last one ends after the time it carries (TIME or PARKING_TIME).
func periodEntries(session map[string]any) []periodEntry {
	periods, _ := session["charging_periods"].([]any)
	entries := make([]periodEntry, 0, len(periods))
	for _, raw := range periods {
		period, _ := raw.(map[string]any)
		start, err := parseOCPITime(fieldText(period, "start_date_time"))
		if err != nil {
			continue
		}
		current := periodEntry{start: start}
		hours := 0.0
		dimensions, _ := period["dimensions"].([]any)
		for _, rawDimension := range dimensions {
			dimension, _ := rawDimension.(map[string]any)
			switch strings.ToUpper(fieldText(dimension, "type")) {
			case "ENERGY":
				current.kwh += number(dimension["volume"])
				current.charging = true
			case "TIME":
				hours += number(dimension["volume"])
				current.charging = true
			case "PARKING_TIME":
				hours += number(dimension["volume"])
			}
		}
		current.end = start.Add(time.Duration(hours * float64(time.Hour)))
		entries = append(entries, current)
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].start.Before(entries[j].start) })
	for index := range entries {
		if index+1 < len(entries) {
			entries[index].end = entries[index+1].start
		}
		if entries[index].end.Before(entries[index].start) {
			entries[index].end = entries[index].start
		}
	}
	return entries
}

// periodsEnd is where the Session's reported Charging Periods stop: the next period starts here. A
// Session without periods starts reporting at its start_date_time.
func periodsEnd(session map[string]any) time.Time {
	entries := periodEntries(session)
	if len(entries) > 0 {
		return entries[len(entries)-1].end
	}
	start, _ := parseOCPITime(fieldText(session, "start_date_time"))
	return start
}

// sessionSpans rebuilds the charging timeline from a Session's ChargingPeriods. Periods carrying only
// PARKING_TIME are not charging. Charging the meter has reached but no period carries yet — between two
// reported periods, or a hand-edited kwh — runs from the end of the reported periods to chargeEnd.
func sessionSpans(session map[string]any, chargeEnd time.Time) []chargeSpan {
	spans := []chargeSpan{}
	delivered := 0.0
	for _, entry := range periodEntries(session) {
		if !entry.charging {
			continue
		}
		spans = append(spans, chargeSpan{Start: entry.start, End: entry.end, KWh: entry.kwh})
		delivered += entry.kwh
	}
	reported := periodsEnd(session)
	missing := number(session["kwh"]) - delivered
	if missing < 1e-6 {
		missing = 0
	}
	if chargeEnd.Sub(reported) > time.Millisecond || missing > 0 {
		start := reported
		if chargeEnd.Before(start) {
			start = chargeEnd
		}
		spans = append(spans, chargeSpan{Start: start, End: chargeEnd, KWh: missing})
	}
	return spans
}

// chargePeriods are the Charging Periods for the charging since the reported periods ended, up to `at`,
// cut wherever a Tariff Element can become active or inactive (OCPI 2.2.1 §10.3.1: a CPO SHALL start a
// ChargingPeriod at every moment that has relevance for the total costs). The session's kwh must already
// hold the reading at `at`.
func chargePeriods(session map[string]any, tariff map[string]any, tariffID string, zone *time.Location, at time.Time) []any {
	reported := periodsEnd(session)
	sessionStart, err := parseOCPITime(fieldText(session, "start_date_time"))
	if err != nil {
		sessionStart = reported
	}
	delivered := 0.0
	for _, entry := range periodEntries(session) {
		delivered += entry.kwh
	}
	missing := number(session["kwh"]) - delivered
	if missing < 1e-6 {
		missing = 0
	}
	if at.Before(reported) {
		at = reported
	}
	unreported := []chargeSpan{{Start: reported, End: at, KWh: missing}}
	var added []any
	for _, slice := range chargeSlices(tariff, unreported, sessionStart.In(zone), delivered) {
		if slice.KWh <= 0 && !slice.End.After(slice.Start) {
			continue
		}
		volumes := map[string]float64{}
		if slice.KWh > 0 {
			volumes["ENERGY"] = roundHours(slice.KWh)
		}
		if hours := slice.End.Sub(slice.Start).Hours(); hours > 0 {
			volumes["TIME"] = roundHours(hours)
		}
		added = append(added, chargingPeriod(ocpi.FormatTime(slice.Start), tariffID, volumes))
	}
	return added
}

// priceSession bills kwh delivered evenly over hours from at against one Tariff object.
func priceSession(tariff map[string]any, tariffID string, kwh, hours float64, at time.Time) sessionPrice {
	span := chargeSpan{Start: at, End: at.Add(time.Duration(hours * float64(time.Hour))), KWh: kwh}
	return priceSessionWithParking(tariff, tariffID, []chargeSpan{span}, at, span.End, 0)
}

type pricedPiece struct {
	Component map[string]any
	Element   int
	Volume    float64
}

// priceSessionWithParking prices the charging timeline, then the parking that followed it from
// parkingAt. ENERGY and TIME are priced slice by slice, so an element that becomes active or stops
// matching mid-session (clock, duration or energy restriction) bills only its own part.
func priceSessionWithParking(tariff map[string]any, tariffID string, spans []chargeSpan, sessionStart, parkingAt time.Time, parkingHours float64) sessionPrice {
	price := sessionPrice{TariffID: tariffID, Priced: true}
	if tariff == nil {
		return sessionPrice{TariffID: tariffID, Note: "no Tariff object to price against"}
	}
	zone := sessionStart.Location()
	kwh, chargingHours := 0.0, 0.0
	for _, span := range spans {
		kwh += span.KWh
		chargingHours += span.End.Sub(span.Start).Hours()
	}
	segments := parkingSegments(tariff, parkingAt.In(zone), parkingHours, sessionStart, kwh)
	lastBillable := -1
	activeSeconds := 0.0
	for index, segment := range segments {
		if segment.Component != nil {
			lastBillable = index
			activeSeconds += segment.End.Sub(segment.Start).Seconds()
		}
	}

	add := func(dimension string, piece pricedPiece) {
		component := piece.Component
		line := priceLine{Dimension: dimension, Price: number(component["price"]), VAT: number(component["vat"]), StepSize: number(component["step_size"]), Volume: piece.Volume}
		line.ExclVAT = roundMoney(line.Price * line.Volume)
		line.InclVAT = roundMoney(line.ExclVAT * (1 + line.VAT/100))
		price.Lines = append(price.Lines, line)
		price.ExclVAT += line.ExclVAT
		price.InclVAT += line.InclVAT
	}

	if component := priceComponentFor(tariff, "FLAT", sessionStart, kwh, chargingHours+parkingHours); component != nil {
		add("FLAT", pricedPiece{Component: component, Volume: 1})
	}

	var energy, charging []pricedPiece
	for _, slice := range chargeSlices(tariff, spans, sessionStart, 0) {
		mid := slice.Start.Add(slice.End.Sub(slice.Start) / 2)
		midKWh := slice.Delivered + slice.KWh/2
		elapsed := mid.Sub(sessionStart).Hours()
		if component, element := priceComponentAt(tariff, "ENERGY", mid, midKWh, elapsed); component != nil && slice.KWh > 0 {
			energy = appendPiece(energy, pricedPiece{Component: component, Element: element, Volume: slice.KWh})
		}
		if component, element := priceComponentAt(tariff, "TIME", mid, midKWh, elapsed); component != nil && slice.End.After(slice.Start) {
			charging = appendPiece(charging, pricedPiece{Component: component, Element: element, Volume: slice.End.Sub(slice.Start).Hours()})
		}
	}
	// step_size is applied once to the session total, with the step of the last component used, and the
	// rounded-up remainder is billed on that last part (OCPI 2.2.1 §10.4.4, step_size).
	if len(energy) > 0 {
		totalWh := 0.0
		for _, piece := range energy {
			totalWh += piece.Volume * 1000
		}
		last := &energy[len(energy)-1]
		// step_size multiplier for ENERGY is 1 Wh.
		last.Volume += (roundUpToStep(totalWh, number(last.Component["step_size"])) - totalWh) / 1000
	}
	// Charging time followed by billed parking is not rounded: the parking total takes the step.
	if len(charging) > 0 && lastBillable < 0 {
		totalSeconds := 0.0
		for _, piece := range charging {
			totalSeconds += piece.Volume * 3600
		}
		last := &charging[len(charging)-1]
		last.Volume += (roundUpToStep(totalSeconds, number(last.Component["step_size"])) - totalSeconds) / 3600
	}
	for _, piece := range energy {
		add("ENERGY", piece)
	}
	for _, piece := range charging {
		add("TIME", piece)
	}

	if lastBillable >= 0 {
		last := segments[lastBillable].Component
		extraSeconds := roundUpToStep(activeSeconds, number(last["step_size"])) - activeSeconds
		for index, segment := range segments {
			if segment.Component == nil {
				continue
			}
			seconds := segment.End.Sub(segment.Start).Seconds()
			if index == lastBillable {
				seconds += extraSeconds
			}
			add("PARKING_TIME", pricedPiece{Component: segment.Component, Volume: seconds / 3600})
		}
	}
	price.ExclVAT, price.InclVAT = roundMoney(price.ExclVAT), roundMoney(price.InclVAT)
	price, limited := applyPriceLimits(tariff, price)
	if len(price.Lines) == 0 && !limited {
		price.Priced = false
		price.Note = "Tariff " + tariffID + " has no active price component for this session"
	}
	return price
}

// appendPiece merges a slice billed by the same element as the one before it into one line.
func appendPiece(pieces []pricedPiece, piece pricedPiece) []pricedPiece {
	if count := len(pieces); count > 0 && pieces[count-1].Element == piece.Element {
		pieces[count-1].Volume += piece.Volume
		return pieces
	}
	return append(pieces, piece)
}

type chargeSlice struct {
	Start, End time.Time
	KWh        float64
	// Delivered is the session's energy before this slice, which min_kwh/max_kwh are measured against.
	Delivered float64
}

// chargeSlices cuts every charging span wherever a restriction can flip: a clock or day boundary, a
// min/max_duration mark, or the moment the delivered energy crosses a min/max_kwh threshold.
// delivered is the energy charged before the first span.
func chargeSlices(tariff map[string]any, spans []chargeSpan, sessionStart time.Time, delivered float64) []chargeSlice {
	zone := sessionStart.Location()
	thresholds := kwhThresholds(tariff)
	slices := []chargeSlice{}
	for _, span := range spans {
		start, end := span.Start.In(zone), span.End.In(zone)
		duration := end.Sub(start)
		cuts := []float64{0, 1}
		if duration > 0 {
			for _, point := range tariffBoundaries(tariff, start, end, sessionStart) {
				cuts = append(cuts, float64(point.Sub(start))/float64(duration))
			}
		}
		if span.KWh > 0 {
			for _, threshold := range thresholds {
				cuts = append(cuts, (threshold-delivered)/span.KWh)
			}
		}
		sort.Float64s(cuts)
		previous := 0.0
		for _, cut := range cuts {
			if cut <= previous || cut > 1 {
				continue
			}
			slices = append(slices, chargeSlice{
				Start:     start.Add(time.Duration(previous * float64(duration))),
				End:       start.Add(time.Duration(cut * float64(duration))),
				KWh:       span.KWh * (cut - previous),
				Delivered: delivered + span.KWh*previous,
			})
			previous = cut
		}
		delivered += span.KWh
	}
	return slices
}

func kwhThresholds(tariff map[string]any) []float64 {
	var thresholds []float64
	elements, _ := tariff["elements"].([]any)
	for _, raw := range elements {
		element, _ := raw.(map[string]any)
		restrictions, _ := element["restrictions"].(map[string]any)
		for _, key := range []string{"min_kwh", "max_kwh"} {
			if value, ok := restrictions[key]; ok {
				thresholds = append(thresholds, number(value))
			}
		}
	}
	return thresholds
}

// tariffBoundaries lists the instants between from and to at which a clock, day or duration
// restriction of the tariff can start or stop matching.
func tariffBoundaries(tariff map[string]any, from, to, sessionStart time.Time) []time.Time {
	var points []time.Time
	elements, _ := tariff["elements"].([]any)
	for day := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, from.Location()); day.Before(to); day = day.AddDate(0, 0, 1) {
		points = append(points, day.AddDate(0, 0, 1))
		for _, raw := range elements {
			element, _ := raw.(map[string]any)
			restrictions, _ := element["restrictions"].(map[string]any)
			for _, key := range []string{"start_time", "end_time"} {
				if clockText := fieldText(restrictions, key); clockText != "" {
					if parsed, err := time.Parse("15:04", clockText); err == nil {
						points = append(points, time.Date(day.Year(), day.Month(), day.Day(), parsed.Hour(), parsed.Minute(), 0, 0, from.Location()))
					}
				}
			}
		}
	}
	for _, raw := range elements {
		element, _ := raw.(map[string]any)
		restrictions, _ := element["restrictions"].(map[string]any)
		for _, key := range []string{"min_duration", "max_duration"} {
			if value, ok := restrictions[key]; ok {
				points = append(points, sessionStart.Add(time.Duration(number(value)*float64(time.Second))))
			}
		}
	}
	inside := points[:0]
	for _, point := range points {
		if point.After(from) && point.Before(to) {
			inside = append(inside, point)
		}
	}
	return inside
}

type parkingSegment struct {
	Start, End time.Time
	Component  map[string]any
}

// Parking periods split at every tariff clock/day/duration boundary. This keeps the CDR's
// ChargingPeriods aligned with each cost change, including a transition into a free period.
// Without a Tariff the parking is still one unpriced segment: the time happened either way.
func parkingSegments(tariff map[string]any, start time.Time, hours float64, sessionStart time.Time, kwh float64) []parkingSegment {
	if hours <= 0 {
		return nil
	}
	end := start.Add(time.Duration(hours * float64(time.Hour)))
	if tariff == nil {
		return []parkingSegment{{Start: start, End: end}}
	}
	points := append([]time.Time{start, end}, tariffBoundaries(tariff, start, end, sessionStart)...)
	sort.Slice(points, func(i, j int) bool { return points[i].Before(points[j]) })
	segments := []parkingSegment{}
	for index := 1; index < len(points); index++ {
		left, right := points[index-1], points[index]
		if !left.Before(right) {
			continue
		}
		mid := left.Add(right.Sub(left) / 2)
		component := priceComponentFor(tariff, "PARKING_TIME", mid, kwh, mid.Sub(sessionStart).Hours())
		segments = append(segments, parkingSegment{Start: left, End: right, Component: component})
	}
	return segments
}

// priceComponentFor implements the per-dimension lookup of OCPI 2.2.1 §11.4.1: the first element in
// document order that prices this dimension and whose restrictions match.
func priceComponentFor(tariff map[string]any, dimension string, at time.Time, kwh, hours float64) map[string]any {
	component, _ := priceComponentAt(tariff, dimension, at, kwh, hours)
	return component
}

// priceComponentAt is priceComponentFor that also names the element, so slices billed by the same
// element can be told apart from those billed by another one at the same price.
func priceComponentAt(tariff map[string]any, dimension string, at time.Time, kwh, hours float64) (map[string]any, int) {
	elements, _ := tariff["elements"].([]any)
	for index, raw := range elements {
		element, _ := raw.(map[string]any)
		component := priceComponentOfType(element, dimension)
		if component == nil {
			continue
		}
		if !restrictionsMatch(element["restrictions"], at, kwh, hours) {
			continue
		}
		return component, index
	}
	return nil, -1
}

func priceComponentOfType(element map[string]any, dimension string) map[string]any {
	components, _ := element["price_components"].([]any)
	for _, raw := range components {
		component, _ := raw.(map[string]any)
		if strings.EqualFold(fieldText(component, "type"), dimension) {
			return component
		}
	}
	return nil
}

func restrictionsMatch(raw any, at time.Time, kwh, hours float64) bool {
	restrictions, ok := raw.(map[string]any)
	if !ok || len(restrictions) == 0 {
		return true
	}
	for key := range restrictions {
		if !knownRestrictions[key] {
			return false
		}
	}
	if !inClockWindow(clock(at), fieldText(restrictions, "start_time"), fieldText(restrictions, "end_time")) {
		return false
	}
	if days, present := restrictions["day_of_week"].([]any); present {
		today := strings.ToUpper(at.Weekday().String())
		matched := false
		for _, day := range days {
			if strings.EqualFold(strings.TrimSpace(fmt.Sprint(day)), today) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	for field, value := range map[string]float64{"min_kwh": kwh, "max_kwh": kwh, "min_duration": hours * 3600, "max_duration": hours * 3600} {
		bound, present := restrictions[field]
		if !present {
			continue
		}
		if strings.HasPrefix(field, "min_") && value < number(bound) {
			return false
		}
		if strings.HasPrefix(field, "max_") && value >= number(bound) {
			return false
		}
	}
	return true
}

func clock(at time.Time) string { return at.Format("15:04") }

// inClockWindow applies TariffRestrictions start_time/end_time (OCPI 2.2.1 §11.4.6): end_time is
// exclusive, an end_time before start_time wraps around to the next day, and 00:00 ends at midnight.
func inClockWindow(now, start, end string) bool {
	if end == "00:00" {
		end = "24:00"
	}
	switch {
	case start == "" && end == "":
		return true
	case start == "":
		return now < end
	case end == "":
		return now >= start
	case start <= end:
		return now >= start && now < end
	default:
		return now >= start || now < end
	}
}

func roundUpToStep(value, step float64) float64 {
	if step <= 0 || value <= 0 {
		return math.Max(value, 0)
	}
	return math.Ceil(value/step) * step
}

func roundMoney(value float64) float64 {
	rounded, err := strconv.ParseFloat(strconv.FormatFloat(value, 'f', 2, 64), 64)
	if err != nil {
		return value
	}
	return rounded
}

// sessionZone resolves the Location time zone that TariffRestrictions.start_time/end_time are
// expressed in (OCPI 2.2.1 §11.4.6). A location without a usable zone falls back to UTC.
func sessionZone(location map[string]any) *time.Location {
	zone, err := time.LoadLocation(fieldText(location, "time_zone"))
	if err != nil {
		return time.UTC
	}
	return zone
}

// sumDimension adds up one dimension across the Charging Periods already recorded, so a new period
// can carry the delta rather than repeating the running total.
func sumDimension(periods []any, dimension string) float64 {
	total := 0.0
	for _, rawPeriod := range periods {
		period, _ := rawPeriod.(map[string]any)
		dimensions, _ := period["dimensions"].([]any)
		for _, rawDimension := range dimensions {
			entry, _ := rawDimension.(map[string]any)
			if strings.EqualFold(fieldText(entry, "type"), dimension) {
				total += number(entry["volume"])
			}
		}
	}
	return total
}

func chargingPeriod(timestamp, tariffID string, volumes map[string]float64) map[string]any {
	types := make([]string, 0, len(volumes))
	for dimension := range volumes {
		types = append(types, dimension)
	}
	sort.Strings(types)
	dimensions := make([]any, 0, len(types))
	for _, dimension := range types {
		dimensions = append(dimensions, map[string]any{"type": dimension, "volume": volumes[dimension]})
	}
	period := map[string]any{"start_date_time": timestamp, "dimensions": dimensions}
	if tariffID != "" {
		period["tariff_id"] = tariffID
	}
	return period
}
