package declcfg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/text/cases"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/operator-framework/operator-registry/alpha/property"
)

const (
	SchemaPackage = "olm.package"
	SchemaChannel = "olm.channel"
	SchemaBundle  = "olm.bundle"
)

type DeclarativeConfig struct {
	Packages []Package
	Channels []Channel
	Bundles  []Bundle
	Others   []Meta
}

type Package struct {
	Schema         string              `json:"schema"`
	Name           string              `json:"name"`
	DefaultChannel string              `json:"defaultChannel"`
	Icon           *Icon               `json:"icon,omitempty"`
	Description    string              `json:"description,omitempty"`
	Properties     []property.Property `json:"properties,omitempty" hash:"set"`
}

type Icon struct {
	Data      []byte `json:"base64data"`
	MediaType string `json:"mediatype"`
}

type Channel struct {
	Schema     string              `json:"schema"`
	Name       string              `json:"name"`
	Package    string              `json:"package"`
	Entries    []ChannelEntry      `json:"entries"`
	Properties []property.Property `json:"properties,omitempty" hash:"set"`
}

type ChannelEntry struct {
	Name      string   `json:"name"`
	Replaces  string   `json:"replaces,omitempty"`
	Skips     []string `json:"skips,omitempty"`
	SkipRange string   `json:"skipRange,omitempty"`
}

// Bundle specifies all metadata and data of a bundle object.
// Top-level fields are the source of truth, i.e. not CSV values.
//
// Notes:
//   - Any field slice type field or type containing a slice somewhere
//     where two types/fields are equal if their contents are equal regardless
//     of order must have a `hash:"set"` field tag for bundle comparison.
//   - Any fields that have a `json:"-"` tag must be included in the equality
//     evaluation in bundlesEqual().
type Bundle struct {
	Schema        string              `json:"schema"`
	Name          string              `json:"name"`
	Package       string              `json:"package"`
	Image         string              `json:"image"`
	Properties    []property.Property `json:"properties,omitempty" hash:"set"`
	RelatedImages []RelatedImage      `json:"relatedImages,omitempty" hash:"set"`

	// These fields are present so that we can continue serving
	// the GRPC API the way packageserver expects us to in a
	// backwards-compatible way. These are populated from
	// any `olm.bundle.object` properties.
	//
	// These fields will never be persisted in the bundle blob as
	// first class fields.
	CsvJSON string   `json:"-"`
	Objects []string `json:"-"`
}

type RelatedImage struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

type Meta struct {
	Schema  string
	Package string
	Name    string

	Blob json.RawMessage
}

func (m Meta) MarshalJSON() ([]byte, error) {
	return m.Blob, nil
}

func (m *Meta) UnmarshalJSON(blob []byte) error {
	blobMap := map[string]interface{}{}
	if err := json.Unmarshal(blob, &blobMap); err != nil {
		return err
	}

	// TODO: this function ensures we do not break backwards compatibility with
	//    the documented examples of FBC templates, which use upper camel case
	//    for JSON field names. We need to decide if we want to continue supporting
	//    case insensitive JSON field names, or if we want to enforce a specific
	//    case-sensitive key value for each field.
	if err := extractUniqueMetaKeys(blobMap, m); err != nil {
		return err
	}

	buf := bytes.Buffer{}
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(blobMap); err != nil {
		return err
	}
	m.Blob = buf.Bytes()
	return nil
}

// extractUniqueMetaKeys enables a case-insensitive key lookup for the schema, package, and name
// fields of the Meta struct. If the blobMap contains duplicate keys (that is, keys have the same folded value),
// an error is returned.
func extractUniqueMetaKeys(blobMap map[string]any, m *Meta) error {
	keySets := map[string]sets.Set[string]{}
	folder := cases.Fold()
	for key := range blobMap {
		foldKey := folder.String(key)
		if _, ok := keySets[foldKey]; !ok {
			keySets[foldKey] = sets.New[string]()
		}
		keySets[foldKey].Insert(key)
	}

	dupErrs := []error{}
	for foldedKey, keys := range keySets {
		if len(keys) != 1 {
			dupErrs = append(dupErrs, fmt.Errorf("duplicate keys for key %q: %v", foldedKey, sets.List(keys)))
		}
	}
	if len(dupErrs) > 0 {
		return utilerrors.NewAggregate(dupErrs)
	}

	metaMap := map[string]*string{
		folder.String("schema"):  &m.Schema,
		folder.String("package"): &m.Package,
		folder.String("name"):    &m.Name,
	}

	for foldedKey, ptr := range metaMap {
		// if the folded key doesn't exist in the key set derived from the blobMap, that means
		// the key doesn't exist in the blobMap, so we can skip it
		if _, ok := keySets[foldedKey]; !ok {
			continue
		}

		// reset key to the unfolded key, which we know is the one that appears in the blobMap
		key := keySets[foldedKey].UnsortedList()[0]
		if _, ok := blobMap[key]; !ok {
			continue
		}
		v, ok := blobMap[key].(string)
		if !ok {
			return fmt.Errorf("expected value for key %q to be a string, got %t: %v", key, blobMap[key], blobMap[key])
		}
		*ptr = v
	}
	return nil
}

func resolveUnmarshalErr(data []byte, err error) string {
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) {
		return formatUnmarshallErrorString(data, te.Error(), te.Offset)
	}
	var se *json.SyntaxError
	if errors.As(err, &se) {
		return formatUnmarshallErrorString(data, se.Error(), se.Offset)
	}
	return err.Error()
}

func formatUnmarshallErrorString(data []byte, errmsg string, offset int64) string {
	sb := new(strings.Builder)
	_, _ = sb.WriteString(fmt.Sprintf("%s at offset %d (indicated by <==)\n ", errmsg, offset))
	// attempt to present the erroneous JSON in indented, human-readable format
	// errors result in presenting the original, unformatted output
	var pretty bytes.Buffer
	err := json.Indent(&pretty, data, "", "    ")
	if err == nil {
		pString := pretty.String()
		// calc the prettified string offset which correlates to the original string offset
		var pOffset, origOffset int64
		origOffset = 0
		for origOffset = 0; origOffset < offset; {
			if pString[pOffset] != '\n' && pString[pOffset] != ' ' {
				origOffset++
			}
			pOffset++
		}
		_, _ = sb.WriteString(pString[:pOffset])
		_, _ = sb.WriteString(" <== ")
		_, _ = sb.WriteString(pString[pOffset:])
	} else {
		for i := int64(0); i < offset; i++ {
			_ = sb.WriteByte(data[i])
		}
		_, _ = sb.WriteString(" <== ")
		_, _ = sb.Write(data[offset:])
	}

	return sb.String()
}
