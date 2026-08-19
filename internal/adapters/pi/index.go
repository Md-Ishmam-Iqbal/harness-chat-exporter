package pi

import (
	"bufio"
	"container/heap"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/ishmam-iqbal-sazim/harness-chat-exporter/internal/domain"
)

type nodeMeta struct {
	id       string
	parentID string
	typeName string
	targetID string
	label    *string
}

type indexedRecord struct {
	line      int64
	offset    int64
	length    int64
	header    bool
	tooLarge  bool
	malformed bool
	node      nodeMeta
}

type sessionIndex struct {
	header  sessionHeader
	records []indexedRecord
}

func buildIndex(ctx context.Context, file *os.File, recordLimit int64) (sessionIndex, domain.ParseResult) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return sessionIndex{}, parseError("read_error", err)
	}
	reader := bufio.NewReaderSize(file, 64<<10)
	var index sessionIndex
	result := domain.ParseResult{}
	var offset int64
	var previousV1ID string
	for line := int64(1); ; line++ {
		if err := ctx.Err(); err != nil {
			return sessionIndex{}, parseError("cancelled", err)
		}
		raw, tooLarge, consumed, readErr := readRecord(reader, recordLimit)
		if consumed == 0 && errors.Is(readErr, io.EOF) {
			break
		}
		record := indexedRecord{line: line, offset: offset, length: consumed, tooLarge: tooLarge}
		offset += consumed
		if tooLarge {
			result.Partial = true
			result.Malformed++
			result.Warnings = append(result.Warnings, warning("pi_record_too_large", "size_limit", line, "native record exceeds parser safety ceiling"))
			index.records = append(index.records, record)
		} else {
			var envelope struct {
				Type          string          `json:"type"`
				Version       int             `json:"version"`
				ID            string          `json:"id"`
				ParentID      json.RawMessage `json:"parentId"`
				Timestamp     string          `json:"timestamp"`
				CWD           string          `json:"cwd"`
				ParentSession string          `json:"parentSession"`
				LeafID        string          `json:"leafId"`
				CurrentLeafID string          `json:"currentLeafId"`
				TargetID      string          `json:"targetId"`
				Label         json.RawMessage `json:"label"`
			}
			if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Type == "" {
				record.malformed = true
				result.Malformed++
				result.Partial = true
				result.Warnings = append(result.Warnings, warning("pi_malformed_record", "malformed_record", line, "native record could not be decoded"))
			} else if line == 1 && envelope.Type == "session" {
				record.header = true
				index.header = sessionHeader{Type: envelope.Type, Version: envelope.Version, ID: envelope.ID,
					Timestamp: envelope.Timestamp, CWD: envelope.CWD, ParentSession: envelope.ParentSession,
					LeafID: firstNonEmpty(envelope.LeafID, envelope.CurrentLeafID)}
				if index.header.Version == 0 {
					index.header.Version = 1
				}
			} else {
				if line == 1 {
					result.Partial = true
					result.Warnings = append(result.Warnings, warning("pi_missing_header", "unsupported_format", line, "first native record is not a Pi session header"))
				}
				parentID := rawString(envelope.ParentID)
				id := envelope.ID
				if index.header.Version == 1 {
					if id == "" {
						id = fmt.Sprintf("line:%d", line)
					}
					parentID = previousV1ID
					previousV1ID = id
				}
				record.node = nodeMeta{id: id, parentID: parentID, typeName: envelope.Type, targetID: envelope.TargetID}
				if len(envelope.Label) > 0 && string(envelope.Label) != "null" {
					var label string
					if json.Unmarshal(envelope.Label, &label) == nil {
						record.node.label = &label
					}
				}
			}
			index.records = append(index.records, record)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return sessionIndex{}, parseError("read_error", readErr)
		}
	}
	if len(index.records) == 0 {
		result.Partial = true
		result.Warnings = append(result.Warnings, warning("pi_empty_session", "unsupported_format", 0, "session file is empty"))
	}
	if index.header.Version < 1 || index.header.Version > 3 {
		result.Partial = true
		result.Warnings = append(result.Warnings, warning("pi_unsupported_version", "unsupported_format", 1, "Pi session version is not supported; records are retained as unknown"))
	}
	return index, result
}

func rawString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

type branchInfo struct {
	id       *string
	active   *bool
	parentID *string
	label    *string
}

type graphAnalysis struct {
	order    []int
	branch   map[int]branchInfo
	warnings []domain.Warning
	partial  bool
}

func analyzeGraph(index sessionIndex) graphAnalysis {
	analysis := graphAnalysis{branch: map[int]branchInfo{}}
	if len(index.records) == 0 {
		return analysis
	}
	// Header remains first and non-tree malformed/oversized records participate
	// as source-ordered roots so no native evidence is lost.
	for i, record := range index.records {
		if record.header {
			analysis.order = append(analysis.order, i)
		}
	}

	firstByID := map[string]int{}
	duplicate := map[int]bool{}
	for i, record := range index.records {
		if record.header || record.tooLarge || record.malformed {
			continue
		}
		if record.node.id == "" {
			analysis.partial = true
			analysis.warnings = append(analysis.warnings, warning("pi_missing_entry_id", "invalid_relationship", record.line, "tree entry has no native ID"))
			continue
		}
		if _, exists := firstByID[record.node.id]; exists {
			duplicate[i] = true
			index.records[i].node.id = fmt.Sprintf("%s#line:%d", record.node.id, record.line)
			analysis.partial = true
			analysis.warnings = append(analysis.warnings, warning("pi_duplicate_entry_id", "invalid_relationship", record.line, "duplicate native entry ID was retained with source-position identity"))
			continue
		}
		firstByID[record.node.id] = i
	}

	indegree := make(map[int]int)
	children := make(map[int][]int)
	graphNodes := make([]int, 0, len(firstByID))
	rootCount := 0
	for id, i := range firstByID {
		_ = id
		graphNodes = append(graphNodes, i)
		parentID := index.records[i].node.parentID
		if parentID == "" {
			rootCount++
			continue
		}
		parentIndex, ok := firstByID[parentID]
		if !ok {
			rootCount++
			analysis.partial = true
			analysis.warnings = append(analysis.warnings, warning("pi_missing_parent", "invalid_relationship", index.records[i].line, "tree entry references a missing parent"))
			continue
		}
		indegree[i]++
		children[parentIndex] = append(children[parentIndex], i)
	}
	if rootCount > 1 && index.header.Version >= 2 && index.header.Version <= 3 {
		analysis.partial = true
		analysis.warnings = append(analysis.warnings, warning("pi_multiple_roots", "invalid_relationship", 0, "session tree has multiple roots"))
	}
	for parent := range children {
		sortIntsByRecord(children[parent], index.records)
	}

	queue := &recordHeap{records: index.records}
	heap.Init(queue)
	for _, i := range graphNodes {
		if indegree[i] == 0 {
			heap.Push(queue, i)
		}
	}
	// Records that cannot safely join the parent graph are independent roots.
	// Scheduling them in the same source-position heap preserves native order
	// whenever doing so does not violate a known parent relationship.
	for i, record := range index.records {
		if record.header {
			continue
		}
		if record.malformed || record.tooLarge || record.node.id == "" || duplicate[i] {
			heap.Push(queue, i)
		}
	}
	var topo []int
	seen := map[int]bool{}
	for queue.Len() > 0 {
		i := heap.Pop(queue).(int)
		if seen[i] {
			continue
		}
		seen[i] = true
		topo = append(topo, i)
		for _, child := range children[i] {
			indegree[child]--
			if indegree[child] == 0 {
				heap.Push(queue, child)
			}
		}
	}
	validSeen := 0
	for _, i := range graphNodes {
		if seen[i] {
			validSeen++
		}
	}
	if validSeen != len(graphNodes) {
		analysis.partial = true
		var cyclic []int
		for _, i := range graphNodes {
			if !seen[i] {
				cyclic = append(cyclic, i)
			}
		}
		sortIntsByRecord(cyclic, index.records)
		line := int64(0)
		if len(cyclic) > 0 {
			line = index.records[cyclic[0]].line
		}
		analysis.warnings = append(analysis.warnings, warning("pi_parent_cycle", "invalid_relationship", line, "session tree contains a parent cycle; cyclic entries were retained in source order"))
		topo = append(topo, cyclic...)
	}

	analysis.order = append(analysis.order, topo...)

	labels := map[string]*string{}
	for _, record := range index.records {
		if record.node.typeName == "label" && record.node.targetID != "" {
			labels[record.node.targetID] = record.node.label
		}
	}
	active, activeKnown := activePath(index, firstByID)
	branchIDs := map[int]string{}
	for _, i := range topo {
		record := index.records[i]
		if canonical, ok := firstByID[record.node.id]; !ok || canonical != i {
			continue
		}
		parentIndex, parentKnown := firstByID[record.node.parentID]
		branchID := ""
		if parentKnown {
			branchID = branchIDs[parentIndex]
			if len(children[parentIndex]) > 1 {
				branchID = record.node.id
			}
		} else if rootCount > 1 {
			branchID = record.node.id
		}
		branchIDs[i] = branchID
		info := branchInfo{label: labels[record.node.id]}
		if branchID != "" {
			info.id = stringPointer(branchID)
		}
		if record.node.parentID != "" {
			info.parentID = stringPointer(record.node.parentID)
		}
		if activeKnown {
			value := active[i]
			info.active = boolPointer(value)
		}
		analysis.branch[i] = info
	}
	return analysis
}

func activePath(index sessionIndex, firstByID map[string]int) (map[int]bool, bool) {
	leafID := index.header.LeafID
	if leafID == "" {
		for i := len(index.records) - 1; i >= 0; i-- {
			record := index.records[i]
			if record.node.id != "" && firstByID[record.node.id] == i {
				leafID = record.node.id
				break
			}
		}
	}
	if leafID == "" {
		return nil, false
	}
	active := map[int]bool{}
	visited := map[string]bool{}
	for leafID != "" {
		if visited[leafID] {
			return nil, false
		}
		visited[leafID] = true
		i, ok := firstByID[leafID]
		if !ok {
			return nil, false
		}
		active[i] = true
		leafID = index.records[i].node.parentID
	}
	return active, true
}

func readIndexedRecord(file *os.File, record indexedRecord, recordLimit int64) ([]byte, error) {
	if record.length < 0 || record.length > recordLimit {
		return nil, errors.New("invalid indexed record length")
	}
	data := make([]byte, int(record.length))
	n, err := file.ReadAt(data, record.offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if int64(n) != record.length {
		return nil, io.ErrUnexpectedEOF
	}
	return trimLine(data), nil
}

type recordHeap struct {
	values  []int
	records []indexedRecord
}

func (h recordHeap) Len() int { return len(h.values) }
func (h recordHeap) Less(i, j int) bool {
	return h.records[h.values[i]].line < h.records[h.values[j]].line
}
func (h recordHeap) Swap(i, j int)   { h.values[i], h.values[j] = h.values[j], h.values[i] }
func (h *recordHeap) Push(value any) { h.values = append(h.values, value.(int)) }
func (h *recordHeap) Pop() any {
	old := h.values
	n := len(old)
	value := old[n-1]
	h.values = old[:n-1]
	return value
}

func boolPointer(value bool) *bool { return &value }

var _ heap.Interface = (*recordHeap)(nil)
