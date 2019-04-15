package newrelic

import (
	"encoding/json"
	"time"

	"newrelic/collector"
)

// An SQLId is a unique identifier for an SQL statement. The agent
// is responsible generating these values using an implementation
// defined algorithm.
type SQLId uint32

// A SlowSQL aggregates information about occurrences of an SQL statement
// based on observations reported by an agent. Typically, agents do not
// report all occurrences, so the Count, TotalMicros and MinMicros fields
// should be taken with a grain of salt.
type SlowSQL struct {
	ID          SQLId  // unique identifier generated by the observing agent
	Count       int32  // number of times the query has been observed
	TotalMicros uint64 // cumulative duration (usecs)
	MinMicros   uint64 // minimum observed duration (usecs)
	MaxMicros   uint64 // maximum observed duration (usecs)

	// The following fields contain metadata associated with the SQL statement.
	// When Count > 1, the values are from the slowest observation.

	MetricName string // the datastore metric derived from the statement
	Query      string // the SQL statement
	TxnName    string // the name of the originating transaction
	TxnURL     string // the URI of the originating request

	// Params is a JSON-encoded object containing additional metadata
	// associated with the SQL statement. This includes attributes
	// generated by the agent such as the backtrace, as well as attributes
	// generated by the application and added via an API.
	Params JSONString
}

// SlowSQLs represents a bounded collection of SQL statements built from
// observations reported by agents. When the collection is full, the
// maximum observed duration for each SQL statement is used to implement
// the replacement strategy.
type SlowSQLs struct {
	slowSQLs []*SlowSQL
}

// NewSlowSQLs returns a new, empty collection of SQL statements with
// maximum capacity max.
func NewSlowSQLs(max int) *SlowSQLs {
	return &SlowSQLs{
		slowSQLs: make([]*SlowSQL, 0, max),
	}
}

// merge aggregates the observations from two equivalent SQL statements.
// Two SQL statements are considered equivalent if they have the same
// unique identifier. The reporting agents are responsible for ensuring
// this invariant is held.
func (slow *SlowSQL) merge(other *SlowSQL) {
	slow.Count += other.Count
	slow.TotalMicros += other.TotalMicros

	if other.MinMicros < slow.MinMicros {
		slow.MinMicros = other.MinMicros
	}
	if other.MaxMicros > slow.MaxMicros {
		slow.MaxMicros = other.MaxMicros

		// take the sql and other fields from the slowest instance
		slow.Query = other.Query
		slow.MetricName = other.MetricName
		slow.Params = other.Params
		slow.TxnName = other.TxnName
		slow.TxnURL = other.TxnURL
	}
}

// fastest returns the index of the SQL statement with the least maximum
// observed duration.
func (slows *SlowSQLs) fastest() (int, bool) {
	var minIdx int
	var minOfMax uint64

	if 0 == len(slows.slowSQLs) {
		return 0, false
	}

	first := true
	for idx, s := range slows.slowSQLs {
		if first || s.MaxMicros < minOfMax {
			minOfMax = s.MaxMicros
			minIdx = idx
			first = false
		}
	}
	return minIdx, true
}

func (slows *SlowSQLs) find(id SQLId) *SlowSQL {
	for _, slow := range slows.slowSQLs {
		if slow.ID == id {
			return slow
		}
	}
	return nil
}

// Observe aggregates an SQL statement into the collection if the query has
// previously been observed or the collection has sufficient capacity to
// add it. Otherwise, the SQL statement is added conditionally based on the
// collection's replacement strategy.
func (slows *SlowSQLs) Observe(slow *SlowSQL) {
	if existing := slows.find(slow.ID); existing != nil {
		existing.merge(slow)
		return
	}
	if len(slows.slowSQLs) == cap(slows.slowSQLs) {
		if minIdx, ok := slows.fastest(); ok {
			if slows.slowSQLs[minIdx].MaxMicros < slow.MaxMicros {
				slows.slowSQLs[minIdx] = slow
			}
		}
		return
	}
	slows.slowSQLs = append(slows.slowSQLs, slow)
}

func (slow *SlowSQL) collectorParams(compressEncode bool) interface{} {
	if !compressEncode {
		return slow.Params
	}
	p, _ := collector.CompressEncode(slow.Params)
	return p
}

func (slow *SlowSQL) collectorJSON(compressEncode bool) []interface{} {
	return []interface{}{
		slow.TxnName,
		slow.TxnURL,
		slow.ID,
		slow.Query,
		slow.MetricName,
		slow.Count,
		float32(slow.TotalMicros) / 1000.0,
		float32(slow.MinMicros) / 1000.0,
		float32(slow.MaxMicros) / 1000.0,
		slow.collectorParams(compressEncode),
	}
}

// CollectorJSON marshals the collection of slow SQL statement s into JSON
// according to the schema expected by the collector.
//
// Note: This JSON does not contain the agentRunID.  This is for
// historical reasons. Since the agentRunID is included in the url,
// its use in the other commands' JSON is admittedly redundant,
// although required.
func (slows *SlowSQLs) CollectorJSON(compressEncode bool) ([]byte, error) {
	inner := make([][]interface{}, len(slows.slowSQLs))

	for i, s := range slows.slowSQLs {
		inner[i] = s.collectorJSON(compressEncode)
	}

	outer := [...]interface{}{inner}

	return json.Marshal(outer)
}

// Empty returns true if the collection is empty.
func (slows *SlowSQLs) Empty() bool {
	return 0 == len(slows.slowSQLs)
}

// Data marshals the collection of slow SQL statements into JSON according
// to the schema expected by the collector.
func (slows *SlowSQLs) Data(id AgentRunID, harvestStart time.Time) ([]byte, error) {
	return slows.CollectorJSON(true)
}

// Audit marshals the collection of slow SQL statements into JSON according
// to the schema expected by the audit log. This is the same schema
// expected by the collector except compression and base64 encoding is
// disabled to aid readability.
func (slows *SlowSQLs) Audit(id AgentRunID, harvestStart time.Time) ([]byte, error) {
	return slows.CollectorJSON(false)
}

// FailedHarvest discards the collection of slow SQL statements after
// an attempt to send them to the collector fails.
func (slows *SlowSQLs) FailedHarvest(newHarvest *Harvest) {
}
