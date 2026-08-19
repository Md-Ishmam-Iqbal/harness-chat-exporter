package pi

import "testing"

func FuzzGraphTraversal(f *testing.F) {
	f.Add([]byte{0, 1, 2, 1, 3, 0})
	f.Add([]byte{1, 1, 1, 1})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 256 {
			data = data[:256]
		}
		index := sessionIndex{header: sessionHeader{Version: 3}}
		index.records = append(index.records, indexedRecord{line: 1, header: true})
		for i, value := range data {
			id := "node-" + decimal(i)
			parent := ""
			if i > 0 {
				switch value % 5 {
				case 0:
					parent = "node-" + decimal(i-1)
				case 1:
					parent = "missing"
				case 2:
					parent = id
				case 3:
					id = "duplicate"
				case 4:
					parent = "node-" + decimal(int(value)%i)
				}
			}
			index.records = append(index.records, indexedRecord{
				line: int64(i + 2), node: nodeMeta{id: id, parentID: parent, typeName: "message"},
			})
		}
		analysis := analyzeGraph(index)
		if len(analysis.order) > len(index.records) {
			t.Fatalf("order grew beyond indexed records: %d > %d", len(analysis.order), len(index.records))
		}
		seen := map[int]bool{}
		for _, record := range analysis.order {
			if record < 0 || record >= len(index.records) {
				t.Fatalf("record index out of bounds: %d", record)
			}
			if seen[record] {
				t.Fatalf("record index emitted twice: %d", record)
			}
			seen[record] = true
		}
	})
}
