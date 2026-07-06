package avatar

import (
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	indentSVGOutput = ""
	svgContentType  = "image/svg+xml"

	cozyPersonPath = "M12 12c2.21 0 4-1.79 4-4s-1.79-4-4-4-4 1.79-4 4 1.79 4 4 4zm0 2c-2.67 0-8 1.34-8 4v2h16v-2c0-2.66-5.33-4-8-4z"

	cozyUIInitialsColorDarkMode  = "rgba(66, 66, 68, 0.9)"
	cozyUIInitialsColorLightMode = "#ffffff"

	cozyUIFontFamily    = "Lato"
	cozyUIFontFallbacks = ", sans-serif"
	cozyUIFontCSS       = "font-family:" + cozyUIFontFamily + cozyUIFontFallbacks + "; font-weight: 600;"
)

type linearGradientStop struct {
	OffsetPerc float64
	Color      string
}

type linearGradient struct {
	CSSAngleDeg int
	Stops       []linearGradientStop
}

type fontDescriptor struct{ name, style, weight string }

// For the moment, there's only one font anyway
var fontKey = fontDescriptor{cozyUIFontFamily, "normal", "bold"}

type avatarFontProvider func(familyName, style, weight string) ([]byte, error)

type avatarInfo struct {
	initials   string
	gradient   *linearGradient
	grayscale  bool
	faded      bool
	fontLoader avatarFontProvider
}

// grep linear-gradient path-to/cozy-ui/react/Avatar/helpers.js
var cozyUIAvatarColorSchemes = []*linearGradient{
	{CSSAngleDeg: 136, Stops: []linearGradientStop{{OffsetPerc: 14.84, Color: "#F8D280"}, {OffsetPerc: 96.03, Color: "#F2AC69"}}},
	{CSSAngleDeg: 136, Stops: []linearGradientStop{{OffsetPerc: 14.84, Color: "#81EAD4"}, {OffsetPerc: 96.03, Color: "#62C6B7"}}},
	{CSSAngleDeg: 136, Stops: []linearGradientStop{{OffsetPerc: 14.84, Color: "#F19E86"}, {OffsetPerc: 96.03, Color: "#F95967"}}},
	{CSSAngleDeg: 136, Stops: []linearGradientStop{{OffsetPerc: 14.84, Color: "#E4ABF0"}, {OffsetPerc: 96.03, Color: "#D96EED"}}},
	{CSSAngleDeg: 136, Stops: []linearGradientStop{{OffsetPerc: 14.84, Color: "#85D9FD"}, {OffsetPerc: 96.03, Color: "#2A9EFC"}}},
	{CSSAngleDeg: 136, Stops: []linearGradientStop{{OffsetPerc: 39.32, Color: "#80AEFF"}, {OffsetPerc: 96.03, Color: "#883DFE"}}},
	{CSSAngleDeg: 136, Stops: []linearGradientStop{{OffsetPerc: 14.84, Color: "#BDF4A1"}, {OffsetPerc: 96.03, Color: "#52CE64"}}},
	{CSSAngleDeg: 136, Stops: []linearGradientStop{{OffsetPerc: 14.84, Color: "#A1D6F4"}, {OffsetPerc: 96.03, Color: "#52CEC2"}}},
	{CSSAngleDeg: 135, Stops: []linearGradientStop{{OffsetPerc: 00.00, Color: "#86D9D3"}, {OffsetPerc: 100.0, Color: "#1CCFB4"}}},
	{CSSAngleDeg: 135, Stops: []linearGradientStop{{OffsetPerc: 00.00, Color: "#E2FA17"}, {OffsetPerc: 100.0, Color: "#75D8CB"}}},
	{CSSAngleDeg: 136, Stops: []linearGradientStop{{OffsetPerc: 14.84, Color: "#6DCFFF"}, {OffsetPerc: 96.03, Color: "#3D88F8"}}},
}

// Encode an unclosed XML tag with the provided attributes
func encodeXMLElement(encoder *xml.Encoder, name string, attrs ...xml.Attr) (*xml.StartElement, error) {
	result := &xml.StartElement{Name: xml.Name{Local: name}, Attr: attrs}
	if err := encoder.EncodeToken(*result); err != nil {
		return nil, err
	}
	return result, nil
}

// Encode the opening and closing XML tags for an element, which
// is provided to the `body` callback to add child elements or text
func encodeClosedXMLElement(encoder *xml.Encoder, name string, body func(encoder *xml.Encoder) error, attrs ...xml.Attr) error {
	element, err := encodeXMLElement(encoder, name, attrs...)
	if err != nil {
		return err
	}
	if body != nil {
		err := body(encoder)
		if err != nil {
			return err
		}
	}
	return encoder.EncodeToken(element.End())
}

// Create a suitable callback for the `body` parameter of `encodeClosedXMLElement`
// that will add the provided text as XML CharData as the contents of that element
func makeCharDataEncoder(text string) func(encoder *xml.Encoder) error {
	return func(encoder *xml.Encoder) error {
		return encoder.EncodeToken(xml.CharData(text))
	}
}
func makeXMLAttr(name, val string) xml.Attr {
	return xml.Attr{Name: xml.Name{Local: name}, Value: val}
}
func makeXMLAttrf(name, valFormat string, args ...any) xml.Attr {
	return makeXMLAttr(name, fmt.Sprintf(valFormat, args...))
}
func makeIntPxXMLAttr(name string, val int) xml.Attr {
	return makeXMLAttrf(name, "%dpx", val)
}

//	  <linearGradient id="bkg" ...>
//		  <stop offset="14.84%" stop-color="blue"></stop>
//		  <stop offset="96.03%" stop-color="red"></stop>
//	  </linearGradient>
func encodeLinearGradient(encoder *xml.Encoder, gradient *linearGradient) error {
	return encodeClosedXMLElement(encoder, "linearGradient", func(encoder *xml.Encoder) error {
		for _, stop := range gradient.Stops {
			if err := encodeClosedXMLElement(encoder, "stop", nil,
				makeXMLAttrf("offset", "%.2f%%", stop.OffsetPerc),
				makeXMLAttr("stop-color", stop.Color),
			); err != nil {
				return err
			}
		}
		return nil
	},
		makeXMLAttr("id", "bkg"),
		makeXMLAttr("userSpaceOnUse", "userSpaceOnUse"),
		// the -45 is because the (x1,y1)-(x2,y2) is at 45º but needed for the proper stretch to match the CSS
		// the -90 is to fix the quadrant for svg coordinates
		makeXMLAttr("gradientTransform", fmt.Sprintf("rotate(%d 0.5 0.5)", gradient.CSSAngleDeg-90-45)),
		makeXMLAttr("x1", "0%"), makeXMLAttr("y1", "0%"),
		makeXMLAttr("x2", "100%"), makeXMLAttr("y2", "100%"),
	)
}

// Deterministically pick one of the `cozyUIAvatarColorSchemes` for a given `gradientHash` number
func getGradientByHash(gradientHash int) *linearGradient {
	if gradientHash < 0 {
		gradientHash = -gradientHash
	}
	return cozyUIAvatarColorSchemes[gradientHash%len(cozyUIAvatarColorSchemes)]
}

var fontMap = make(map[fontDescriptor]string)

//	 <style>
//			@font-face{
//				font-family:"Roboto Condensed";
//				src:url(data:application/font-woff;charset=utf-8;base64,font_data_in_base64) format("woff");
//				font-weight:normal;font-style:normal;
//			}
//			.cozy-stack-avatar ... { ...
//
// }
func encodeStyleElement(encoder *xml.Encoder, avatar *avatarInfo) error {
	fontBase64 := fontMap[fontKey]
	if fontBase64 == "" && avatar.fontLoader != nil {
		fontData, err := avatar.fontLoader(fontKey.name, fontKey.style, fontKey.weight)
		if err != nil {
			return err
		}
		fontBase64 = base64.StdEncoding.EncodeToString(fontData)
		fontMap[fontKey] = fontBase64
	}
	removeTabs := func(s string) string {
		return strings.ReplaceAll(s, "\t", "")
	}
	css := ""
	if fontBase64 != "" {
		css += removeTabs(fmt.Sprintf(`
			@font-face{font-family: "%s";
			src:url(data:application/font-woff;charset=utf-8;base64,%s) format("woff");
			font-weight:%s;font-style:%s;
		}`, fontKey.name, fontBase64, fontKey.weight, fontKey.style)) + "\n"
	}
	css += removeTabs(fmt.Sprintf(`
		.cozy-stack-avatar text, .cozy-stack-avatar path { font-size: 16px; user-select: none; %s; fill: %s; }
		@media (prefers-color-scheme: dark) {
			.cozy-stack-avatar text, .cozy-stack-avatar path { fill: %s; }
		}
		`,
		cozyUIFontCSS, cozyUIInitialsColorLightMode, cozyUIInitialsColorDarkMode))

	return encodeClosedXMLElement(encoder, "style", makeCharDataEncoder(css), makeXMLAttr("type", "text/css"))
}

//	  <clipPath id="clip">
//		  <circle cx="16px" cy="16px" r="16px" />
//	  </clipPath>
func encodeClipPath(encoder *xml.Encoder, id string, size int) error {
	if err := encodeClosedXMLElement(encoder, "clipPath",
		func(encoder *xml.Encoder) error {
			return encodeClosedXMLElement(encoder, "circle", nil,
				makeIntPxXMLAttr("cx", size),
				makeIntPxXMLAttr("cy", size),
				makeIntPxXMLAttr("r", size),
			)
		},
		makeXMLAttr("id", id),
	); err != nil {
		return err
	}
	return nil
}

//	 <filter id="grayscale">
//		 <feColorMatrix type="saturate" values="0.05"/>
//	 </filter>
func encodeGrayscaleFilter(encoder *xml.Encoder) error {
	if err := encodeClosedXMLElement(encoder, "filter",
		func(encoder *xml.Encoder) error {
			return encodeClosedXMLElement(encoder, "feColorMatrix", nil,
				makeXMLAttr("type", "saturate"),
				makeXMLAttr("values", "0.05"),
			)
		},
		makeXMLAttr("id", "grayscale"),
	); err != nil {
		return err
	}
	return nil
}

// MarshallXML implements encoding/xml.Marshaller.MarshalXML.
//
//		<svg>
//		  <style...
//		  <defs>
//		    <clipPath id="clip"...
//		    <linearGradient id="bkg"...
//	      <filter id="grayscale"...
//		  </defs>
//		  <g>
//		    <circle...
//
//		    <path...
//		    or
//		    <text...
func (a *avatarInfo) MarshalXML(encoder *xml.Encoder, _ xml.StartElement) error {
	svgElement, err := encodeXMLElement(encoder, "svg",
		makeXMLAttr("style", "shape-rendering:geometricPrecision; text-rendering:geometricPrecision; image-rendering:optimizeQuality;"),
		makeXMLAttrf("viewBox", "0 0 32 32"),
		makeXMLAttr("version", "1.1"),
		makeXMLAttr("xmlns", "http://www.w3.org/2000/svg"),
	)
	if err != nil {
		return nil
	}
	if err = encodeStyleElement(encoder, a); err != nil {
		return err
	}

	if err = encodeClosedXMLElement(encoder, "defs",
		func(encoder *xml.Encoder) error {
			err := encodeClipPath(encoder, "clip", 16)
			if err != nil {
				return err
			}
			if a.grayscale {
				err = encodeGrayscaleFilter(encoder)
				if err != nil {
					return err
				}
			}
			return encodeLinearGradient(encoder, a.gradient)
		}); err != nil {
		return err
	}

	gAttributes := make([]xml.Attr, 1, 3)
	gAttributes[0] = makeXMLAttr("class", "cozy-stack-avatar")
	if a.grayscale {
		gAttributes = append(gAttributes, makeXMLAttr("filter", "url(#grayscale)"))
	}
	if a.faded {
		gAttributes = append(gAttributes, makeXMLAttr("style", "opacity: 0.3;"))
	}

	if err = encodeClosedXMLElement(encoder, "g",
		func(encoder *xml.Encoder) error {
			if err = encodeClosedXMLElement(encoder, "circle", nil,
				makeXMLAttr("fill", "url(#bkg)"),
				makeIntPxXMLAttr("cx", 16),
				makeIntPxXMLAttr("cy", 16),
				makeIntPxXMLAttr("r", 16),
			); err != nil {
				return err
			}

			if a.initials == "" {
				// halfCozyPersonSize := cozyPersonPathSize / 2
				if err = encodeClosedXMLElement(encoder, "path", nil,
					makeXMLAttr("d", cozyPersonPath),
					makeXMLAttrf("transform", "translate(4 4)"),
				); err != nil {
					return nil
				}
			} else {
				if err = encodeClosedXMLElement(encoder, "text", makeCharDataEncoder(a.initials),
					makeXMLAttr("clip-path", "url(#clip)"),
					makeXMLAttr("text-anchor", "middle"),
					makeIntPxXMLAttr("x", 16),
					makeIntPxXMLAttr("y", 22), // hardcoded vertical center to support every browsers
				); err != nil {
					return err
				}
			}
			return nil
		},
		gAttributes...,
	); err != nil {
		return err
	}

	return encoder.EncodeToken(svgElement.End())
}

// Stream the SVG (XML) for this `avatarInfo` into `w`
func (a *avatarInfo) writeTo(w io.Writer) error {
	// writeTo(w Writer) (n int64, err error) - would need to calculate size to implement interface correctly
	// but the generated SVGs are tiny, it's not worth streaming outside of the encoding
	encoder := xml.NewEncoder(w)
	encoder.Indent("", indentSVGOutput)
	return encoder.Encode(a)
}

// Get the SVG (XML) for this `avatarInfo` as a string
func (a *avatarInfo) String() (string, error) {
	var builder strings.Builder
	err := a.writeTo(&builder)
	if err != nil {
		return "", err
	}
	return builder.String(), nil
}

var ErrInvalidSizeArg = errors.New("invalid size name")

// Return the SVG XML body for the given initials, the content-type and an error
func SvgForAvatar(initials string, gradientHash uint, grayscale bool, faded bool, fontLoader avatarFontProvider) ([]byte, string, error) {
	gradient := getGradientByHash(int(gradientHash))
	avatar := &avatarInfo{initials: initials, gradient: gradient, grayscale: grayscale, faded: faded, fontLoader: fontLoader}
	svg, err := avatar.String()
	return []byte(svg), svgContentType, err
}
