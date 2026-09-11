package signatures

import (
	"bytes"
	"fmt"
	"go.yaml.in/yaml/v3"
	"io"
)

// BotDefinition is the shared ordered bot catalog. Consumers choose whether
// unmatched/browser observations belong to their dataset.
type BotDefinition struct {
	Rules struct {
		EmptyUA struct {
			Operator string `yaml:"operator"`
			Category string `yaml:"category"`
		} `yaml:"empty_ua"`
		Installers []struct {
			Prefix string `yaml:"prefix"`
			Label  string `yaml:"label"`
		} `yaml:"installers"`
		GenericTokens []string `yaml:"generic_tokens"`
		Generic       struct {
			Operator string `yaml:"operator"`
			Category string `yaml:"category"`
		} `yaml:"generic"`
		BrowserMarkers struct {
			AllOf []string `yaml:"all_of"`
			AnyOf []string `yaml:"any_of"`
		} `yaml:"browser_markers"`
		Unidentified struct {
			Operator string `yaml:"operator"`
			Category string `yaml:"category"`
		} `yaml:"unidentified"`
	} `yaml:"rules"`
	Signatures []struct {
		Needle   string `yaml:"needle"`
		Operator string `yaml:"operator"`
		Category string `yaml:"category"`
	} `yaml:"signatures"`
	CategoryLabels map[string]string `yaml:"category_labels"`
}

// LoadBots rejects unknown fields and extra documents, so browser and server
// classification cannot silently parse different subsets of the catalog.
func LoadBots() (BotDefinition, error) {
	var result BotDefinition
	decoder := yaml.NewDecoder(bytes.NewReader(Bots()))
	decoder.KnownFields(true)
	if err := decoder.Decode(&result); err != nil {
		return result, fmt.Errorf("load bots.yaml: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return result, fmt.Errorf("load bots.yaml: multiple YAML documents")
		}
		return result, fmt.Errorf("load bots.yaml: %w", err)
	}
	return result, nil
}
