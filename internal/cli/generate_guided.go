package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/antchfx/xmlquery"
	"github.com/readium/go-toolkit/pkg/fetcher"
	"github.com/readium/go-toolkit/pkg/guidednavigation"
	"github.com/readium/go-toolkit/pkg/guidednavigation/converter"
	"github.com/readium/go-toolkit/pkg/manifest"
	"github.com/readium/go-toolkit/pkg/mediatype"
	"github.com/readium/go-toolkit/pkg/parser/epub"
	"github.com/readium/go-toolkit/pkg/pub"
	"github.com/readium/go-toolkit/pkg/util/url"
	"golang.org/x/net/html"
)

const guidedNavigationFilename = "guided-navigation.json"

// One document follows the publication's reading order. All references remain
// relative to the publication root, alongside the generated manifest.
func materializeGuidedNavigation(ctx context.Context, p *pub.Publication) (*guidednavigation.GuidedNavigationDocument, error) {
	doc := &guidednavigation.GuidedNavigationDocument{Guided: []guidednavigation.GuidedNavigationObject{}}
	htmlService, _ := p.FindService(pub.GuidedNavigationService_Name).(pub.GuidedNavigationService)
	index := &guideHTMLIndex{publication: p, documents: map[string]*html.Node{}}
	var total float64
	completeDuration, synchronized := true, false
	allAudio := len(p.Manifest.ReadingOrder) > 0
	for i := range p.Manifest.ReadingOrder {
		link := &p.Manifest.ReadingOrder[i]
		var objects []guidednavigation.GuidedNavigationObject
		if smil := findLinkByMediaType(link.Alternates, mediatype.SMIL.String()); smil != nil {
			res := p.Get(ctx, *smil)
			n, rerr := fetcher.ReadResourceAsXML(ctx, res)
			res.Close()
			if rerr != nil {
				return nil, fmt.Errorf("read %s: %v", smil.Href.String(), rerr)
			}
			var err error
			objects, err = orderedSMIL(n, smil.URL(nil, nil))
			if err != nil {
				return nil, fmt.Errorf("convert %s: %w", smil.Href.String(), err)
			}
			if len(objects) == 0 {
				return nil, fmt.Errorf("empty media overlay: %s", smil.Href.String())
			}
			if err := index.enrich(ctx, objects); err != nil {
				return nil, err
			}
			if link.Duration == 0 {
				link.Duration = smil.Duration
			}
		} else if htmlService != nil && htmlService.HasGuideForResource(link.Href.String()) {
			guide, err := htmlService.GuideForResource(ctx, link.Href.String())
			if err != nil {
				return nil, fmt.Errorf("convert %s: %w", link.Href.String(), err)
			}
			if guide != nil {
				objects = guide.Guided
			}
		}
		if len(objects) == 0 {
			object := guidednavigation.GuidedNavigationObject{}
			switch {
			case link.MediaType != nil && link.MediaType.IsAudio():
				object.AudioRef, object.Role = link.URL(nil, nil), []guidednavigation.GuidedNavigationRole{guidednavigation.RoleAudio}
			case link.MediaType != nil && link.MediaType.IsImage():
				object.ImgRef, object.Role = link.URL(nil, nil), []guidednavigation.GuidedNavigationRole{guidednavigation.RoleImage}
			case link.MediaType != nil && link.MediaType.IsVideo():
				object.VideoRef, object.Role = link.URL(nil, nil), []guidednavigation.GuidedNavigationRole{guidednavigation.RoleVideo}
			default:
				object.TextRef, object.Role = link.URL(nil, nil), []guidednavigation.GuidedNavigationRole{guidednavigation.RoleBody}
			}
			objects = []guidednavigation.GuidedNavigationObject{object}
		}
		duration, known, hasAudio, hasSync := guideAudioSummary(objects)
		synchronized = synchronized || hasSync
		if link.Duration == 0 && hasAudio && known {
			link.Duration = duration
		}
		if hasAudio {
			completeDuration = completeDuration && link.Duration > 0
			total += link.Duration
		}
		allAudio = allAudio && link.MediaType != nil && link.MediaType.IsAudio()
		if link.Title == "" {
			link.Title = guideTitle(p.Manifest.TableOfContents, link.Href.String())
			if link.Title == "" && link.MediaType != nil && link.MediaType.IsHTML() {
				n, err := index.load(ctx, link.URL(nil, nil))
				if err != nil {
					return nil, err
				}
				link.Title = htmlTitle(n)
			}
		}
		doc.Guided = append(doc.Guided, objects...)
	}
	if completeDuration && total > 0 && p.Manifest.Metadata.Duration == nil {
		p.Manifest.Metadata.Duration = &total
	}
	static := manifest.Link{
		Href:      manifest.MustNewHREFFromString(guidedNavigationFilename, false),
		MediaType: &mediatype.ReadiumGuidedNavigationDocument,
	}
	if completeDuration {
		static.Duration = total
	}
	// Remove server-only guide templates and replace generated alternates.
	p.Manifest.Links = replaceGuideLinks(p.Manifest.Links, static)
	p.Manifest.Resources = replaceGuideLinks(p.Manifest.Resources, static)
	for i := range p.Manifest.ReadingOrder {
		link := &p.Manifest.ReadingOrder[i]
		link.Alternates = replaceGuideLinks(link.Alternates, static)
	}
	inferred := &manifest.A11y{}
	if synchronized {
		inferred.Features = append(inferred.Features, manifest.A11yFeatureSynchronizedAudioText)
	}
	if len(p.Manifest.TableOfContents) > 0 {
		inferred.Features = append(inferred.Features, manifest.A11yFeatureTableOfContents)
	}
	for _, pages := range p.Manifest.Subcollections["pageList"] {
		if len(pages.Links) > 0 {
			inferred.Features = append(inferred.Features, manifest.A11yFeaturePageNavigation)
			break
		}
	}
	// Media overlays alone do not prove that all intellectual content (including
	// images) is available in audio. Preserve EPUB declarations; only infer this
	// combination for an entirely audio reading order.
	if allAudio {
		inferred.AccessModesSufficient = [][]manifest.A11yPrimaryAccessMode{{manifest.A11yPrimaryAccessModeAuditory}}
	}
	if !inferred.IsEmpty() {
		if p.Manifest.Metadata.Accessibility == nil {
			p.Manifest.Metadata.Accessibility = &manifest.A11y{}
		}
		p.Manifest.Metadata.Accessibility.Merge(inferred)
	}
	return doc, nil
}

func replaceGuideLinks(links manifest.LinkList, replacement manifest.Link) manifest.LinkList {
	result := make(manifest.LinkList, 0, len(links)+1)
	for _, link := range links {
		if link.MediaType == nil || !link.MediaType.Equal(&mediatype.ReadiumGuidedNavigationDocument) {
			result = append(result, link)
		}
	}
	return append(result, replacement)
}

func guideTitle(links manifest.LinkList, href string) string {
	for _, link := range links {
		if strings.SplitN(link.Href.String(), "#", 2)[0] == strings.SplitN(href, "#", 2)[0] && link.Title != "" {
			return link.Title
		}
		if title := guideTitle(link.Children, href); title != "" {
			return title
		}
	}
	return ""
}

// Do not emit a partial sum when a clip has no known end.
func guideAudioSummary(objects []guidednavigation.GuidedNavigationObject) (duration float64, known, audio, synchronized bool) {
	known = true
	for _, object := range objects {
		if object.AudioRef != nil {
			audio = true
			synchronized = synchronized || object.TextRef != nil || !object.Text.Empty()
			clip := object.AudioClip()
			if clip == nil || clip.End == nil {
				known = false
			} else {
				begin := 0.0
				if clip.Begin != nil {
					begin = clip.Begin.Seconds()
				}
				if clip.End.Seconds() < begin {
					known = false
				} else {
					duration += clip.End.Seconds() - begin
				}
			}
		}
		childDuration, childKnown, childAudio, childSync := guideAudioSummary(object.Children)
		duration += childDuration
		known = known && childKnown
		audio, synchronized = audio || childAudio, synchronized || childSync
	}
	return
}

// The toolkit's SMIL sequence parser groups par/seq siblings by element name.
// Walk the source order instead so nested skippable sections stay in place.
func orderedSMIL(n *xmlquery.Node, base url.URL) ([]guidednavigation.GuidedNavigationObject, error) {
	var objects []guidednavigation.GuidedNavigationObject
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if child.Type != xmlquery.ElementNode || (child.NamespaceURI != epub.NamespaceSMIL && child.NamespaceURI != epub.NamespaceSMIL2) {
			continue
		}
		switch child.Data {
		case "par":
			object, err := epub.ParseSMILPar(child, base)
			if err != nil {
				return nil, err
			}
			objects = append(objects, *object)
		case "smil", "body", "seq":
			children, err := orderedSMIL(child, base)
			if err != nil {
				return nil, err
			}
			object := guidednavigation.GuidedNavigationObject{Children: children}
			if ref := epub.SelectNodeAttrNs(child, epub.NamespaceOPS, "textref"); ref != "" {
				u, err := url.URLFromString(ref)
				if err != nil {
					return nil, err
				}
				object.TextRef = base.Resolve(u)
			}
			for _, value := range strings.Fields(epub.SelectNodeAttrNs(child, epub.NamespaceOPS, "type")) {
				if role := converter.ConvertEPUBRole(value); role != "" {
					object.Role = append(object.Role, role)
				}
			}
			if !object.Empty() {
				if object.ChildrenOnly() {
					objects = append(objects, children...)
				} else {
					objects = append(objects, object)
				}
			}
		}
	}
	return objects, nil
}

type guideHTMLIndex struct {
	publication *pub.Publication
	documents   map[string]*html.Node
	targets     map[string]map[string]*html.Node
}

func (g *guideHTMLIndex) load(ctx context.Context, ref url.URL) (*html.Node, error) {
	key := strings.SplitN(ref.String(), "#", 2)[0]
	if n, ok := g.documents[key]; ok {
		return n, nil
	}
	res := g.publication.Get(ctx, manifest.Link{Href: manifest.MustNewHREFFromString(key, false)})
	defer res.Close()
	b, rerr := res.Read(ctx, 0, 0)
	if rerr != nil {
		return nil, fmt.Errorf("read text %s: %v", key, rerr)
	}
	n, err := html.Parse(strings.NewReader(string(b)))
	if err != nil {
		return nil, fmt.Errorf("parse text %s: %w", key, err)
	}
	g.documents[key] = n
	if g.targets == nil {
		g.targets = make(map[string]map[string]*html.Node)
	}
	targets := make(map[string]*html.Node)
	var visit func(*html.Node)
	visit = func(node *html.Node) {
		if node.Type == html.ElementNode {
			if node.Data == "body" {
				targets[""] = node
			}
			for _, attr := range node.Attr {
				if attr.Key == "id" && attr.Val != "" {
					targets[attr.Val] = node
				}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(n)
	g.targets[key] = targets
	return n, nil
}

func (g *guideHTMLIndex) enrich(ctx context.Context, objects []guidednavigation.GuidedNavigationObject) error {
	for i := range objects {
		object := &objects[i]
		if object.TextRef != nil {
			_, err := g.load(ctx, object.TextRef)
			if err != nil {
				return err
			}
			target := g.targets[object.TextFile().String()][object.TextFragmentID()]
			// A span often carries the SMIL target inside a paragraph/heading.
			for target != nil {
				roles := converter.ExtractNodeRoles(target)
				for _, role := range roles {
					if !slices.Contains(object.Role, role) {
						object.Role = append(object.Role, role)
					}
				}
				if len(roles) > 0 {
					break
				}
				target = target.Parent
			}
		}
		if err := g.enrich(ctx, object.Children); err != nil {
			return err
		}
	}
	return nil
}

func findHTMLNode(n *html.Node, match func(*html.Node) bool) *html.Node {
	if n.Type == html.ElementNode && match(n) {
		return n
	}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if found := findHTMLNode(child, match); found != nil {
			return found
		}
	}
	return nil
}

func htmlTitle(n *html.Node) string {
	for _, tag := range []string{"title", "h1", "h2"} {
		node := findHTMLNode(n, func(n *html.Node) bool { return n.Data == tag })
		if node == nil {
			continue
		}
		var text strings.Builder
		var collect func(*html.Node)
		collect = func(n *html.Node) {
			if n.Type == html.TextNode {
				text.WriteString(n.Data)
			}
			for child := n.FirstChild; child != nil; child = child.NextSibling {
				collect(child)
			}
		}
		collect(node)
		if title := strings.Join(strings.Fields(text.String()), " "); title != "" {
			return title
		}
	}
	return ""
}
