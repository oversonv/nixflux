package nixnet

import (
	"encoding/json"
	"fmt"
	"io"

	_ "embed"
)

//go:embed nixnet.json
var embeddedMetadata []byte

type applicationMetadata struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Status  string `json:"status"`
	Data    string `json:"data"`
}

func loadMetadata() (applicationMetadata, error) {
	var metadata applicationMetadata
	if err := json.Unmarshal(embeddedMetadata, &metadata); err != nil {
		return metadata, fmt.Errorf("failed to parse embedded nixnet.json: %w", err)
	}
	return metadata, nil
}

func renderVersion(writer io.Writer) error {
	metadata, err := loadMetadata()
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(
		writer,
		"name: %s\nversion: %s\nstatus: %s\ndata: %s\n",
		metadata.Name,
		metadata.Version,
		metadata.Status,
		metadata.Data,
	)
	return err
}

func renderLegacyVersion(writer io.Writer) error {
	metadata, err := loadMetadata()
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(writer, "%s %s\n", metadata.Name, metadata.Version)
	return err
}
