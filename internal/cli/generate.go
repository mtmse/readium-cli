package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/readium/go-toolkit/pkg/asset"
	"github.com/readium/go-toolkit/pkg/guidednavigation/converter"
	"github.com/readium/go-toolkit/pkg/manifest"
	"github.com/readium/go-toolkit/pkg/mediatype"
	"github.com/readium/go-toolkit/pkg/parser/epub"
	"github.com/readium/go-toolkit/pkg/pub"
	"github.com/readium/go-toolkit/pkg/streamer"
	"github.com/readium/go-toolkit/pkg/util/url"
	"github.com/spf13/cobra"
)

var generateOutDirFlag string
var generateIndentFlag string

var generateCmd = &cobra.Command{
	Use:   "generate <pub-path>",
	Short: "Generate manifest.json, positions.json and guided-navigation.json files",
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return fmt.Errorf("expects a path to the publication")
		}
		if len(args) > 1 {
			return fmt.Errorf("accepts a single path to a publication")
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true
		return generateFiles(args[0], generateOutDirFlag, generateIndentFlag)
	},
}

func init() {
	rootCmd.AddCommand(generateCmd)
	generateCmd.Flags().StringVarP(&generateOutDirFlag, "out", "o", ".", "Output directory")
	generateCmd.Flags().StringVar(&generateIndentFlag, "indent", "  ", "JSON indentation (empty = compact)")
}

func generateFiles(inputPath string, outDir string, indent string) error {
	ctx := context.Background()

	absIn, err := filepath.Abs(inputPath)
	if err != nil {
		return fmt.Errorf("abs input: %w", err)
	}

	u, err := url.FromFilepath(filepath.Clean(absIn))
	if err != nil {
		return fmt.Errorf("url from filepath: %w", err)
	}
	info, err := os.Stat(absIn)
	if err != nil {
		return fmt.Errorf("stat input: %w", err)
	}
	directoryPositions := &directoryPositionStrategy{ctx: ctx}

	// Match how serve opens the publication: service links enabled.
	p, err := streamer.New(streamer.Config{
		AddServiceLinks: true,
		OnCreatePublication: func(b *pub.Builder) error {
			if info.IsDir() && b.Manifest.ConformsTo(manifest.ProfileEPUB) {
				factory := epub.PositionsServiceFactory(directoryPositions)
				b.ServicesBuilder.Set(pub.PositionsService_Name, &factory)
			}
			factory := pub.HTMLGuidedNavigationServiceFactory(converter.WithTextRefLocators())
			b.ServicesBuilder.Set(pub.GuidedNavigationService_Name, &factory)
			// Keep the original SMIL alternates for static, ordered conversion.
			b.ServicesBuilder.Remove(pub.MediaOverlayService_Name)
			return nil
		},
	}).Open(ctx, asset.File(u), "")
	if err != nil {
		return fmt.Errorf("open publication: %w", err)
	}
	defer p.Close()

	// Calculate positions before enriching the shared reading-order links.
	// Their density must not depend on guided navigation or added metadata.
	positionsBytes, err := materializePositionList(ctx, p)
	if err != nil {
		return fmt.Errorf("positions: %w", err)
	}
	if directoryPositions.err != nil {
		return fmt.Errorf("positions: %w", directoryPositions.err)
	}

	guide, err := materializeGuidedNavigation(ctx, p)
	if err != nil {
		return fmt.Errorf("guided navigation: %w", err)
	}
	var guideBytes []byte
	if indent == "" {
		guideBytes, err = json.Marshal(guide)
	} else {
		guideBytes, err = json.MarshalIndent(guide, "", indent)
	}
	if err != nil {
		return fmt.Errorf("marshal guided navigation: %w", err)
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("mkdir out: %w", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, guidedNavigationFilename), guideBytes, 0o644); err != nil {
		return fmt.Errorf("write guided-navigation.json: %w", err)
	}

	positionsPath := filepath.Join(outDir, "positions.json")
	if err := os.WriteFile(positionsPath, positionsBytes, 0o644); err != nil {
		return fmt.Errorf("write positions.json: %w", err)
	}

	// Patch manifest link to point at the static file.
	if err := patchPositionsHref(p, "positions.json"); err != nil {
		return fmt.Errorf("patch manifest: %w", err)
	}

	// Add a relative self link like serve does (but static).
	selfMT := mediatype.ReadiumWebpubManifest
	selfLink := &manifest.Link{
		Rels:      manifest.Strings{"self"},
		MediaType: &selfMT,
		Href:      manifest.MustNewHREFFromString("manifest.json", false),
	}

	var manifestBytes []byte
	if indent == "" {
		manifestBytes, err = json.Marshal(p.Manifest.ToMap(selfLink))
	} else {
		manifestBytes, err = json.MarshalIndent(p.Manifest.ToMap(selfLink), "", indent)
	}
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}

	manifestPath := filepath.Join(outDir, "manifest.json")
	if err := os.WriteFile(manifestPath, manifestBytes, 0o644); err != nil {
		return fmt.Errorf("write manifest.json: %w", err)
	}

	return nil
}

func materializePositionList(ctx context.Context, p *pub.Publication) ([]byte, error) {
	posLink := findLinkByMediaType(p.Manifest.Links, mediatype.ReadiumPositionList.String())
	if posLink == nil {
		return nil, fmt.Errorf("no position-list link found in manifest.links (need AddServiceLinks)")
	}

	// Fetch like serve does for assets: publication.Get(ctx, link) then Read.
	res := p.Get(ctx, *posLink)
	defer res.Close()

	b, rerr := res.Read(ctx, 0, 0)
	if rerr != nil {
		return nil, fmt.Errorf("read position list: %s", rerr.Error())
	}

	// Validate it is JSON (cheap sanity check).
	var tmp any
	if err := json.Unmarshal(b, &tmp); err != nil {
		return nil, fmt.Errorf("position list is not valid JSON: %v", err)
	}
	return b, nil
}

func patchPositionsHref(p *pub.Publication, newHref string) error {
	mt := mediatype.ReadiumPositionList.String()
	for i := range p.Manifest.Links {
		l := p.Manifest.Links[i]
		if l.MediaType != nil && l.MediaType.String() == mt {
			l.Href = manifest.MustNewHREFFromString(newHref, false)
			p.Manifest.Links[i] = l
			return nil
		}
	}
	return fmt.Errorf("no position-list link to patch")
}

func findLinkByMediaType(links manifest.LinkList, mediaType string) *manifest.Link {
	for i := range links {
		l := links[i]
		if l.MediaType != nil && l.MediaType.String() == mediaType {
			return &links[i]
		}
	}
	return nil
}
