package schemas

import "embed"

//go:embed *.schema.json
var Files embed.FS

func ArchiveFiles() (map[string][]byte, error) {
	result := map[string][]byte{}
	for _, name := range []string{"event.schema.json", "manifest.schema.json", "session.schema.json"} {
		data, err := Files.ReadFile(name)
		if err != nil {
			return nil, err
		}
		result["schemas/"+name] = data
	}
	return result, nil
}
