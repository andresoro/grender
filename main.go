package main

import (
	"bytes"
	"flag"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/peterbourgon/mergemap"
	"github.com/russross/blackfriday"
)

var (
	FrontSeparator = []byte("---\n")
)

var (
	debug     = flag.Bool("debug", false, "print debug information")
	sourceDir = flag.String("source", "src", "path to site source (input)")
	targetDir = flag.String("target", "tgt", "path to site target (output)")
	globalKey = flag.String("global.key", "files", "template node name for per-file metadata")
)

func init() {
	flag.Parse()

	var err error
	for _, s := range []*string{sourceDir, targetDir} {
		if *s, err = filepath.Abs(*s); err != nil {
			Fatalf("%s", err)
		}
	}
}

func main() {

	//build site
	m := map[string]interface{}{}
	s := NewStack()
	filepath.Walk(*sourceDir, GatherJSON(s))
	filepath.Walk(*sourceDir, GatherSource(s, m))
	s.Add("", map[string]interface{}{*globalKey: m})
	filepath.Walk(*sourceDir, Transform(s))

	//host site
	fs := http.FileServer(http.Dir(*targetDir))
	http.Handle("/", fs)
	log.Fatal(http.ListenAndServe(":8080", nil))

}

// splitMetadata splits the input buffer on FrontSeparator. It returns a byte-
// slice suitable for unmarshaling into metadata, if it exists, and the
// remainder of the input buffer.
func splitMetadata(buf []byte) ([]byte, []byte) {
	split := bytes.SplitN(buf, FrontSeparator, 2)
	if len(split) == 2 {
		return split[0], split[1]
	}
	return []byte{}, buf
}

func GatherJSON(s StackReadWriter) filepath.WalkFunc {
	Debugf("gathering JSON")
	return func(path string, info os.FileInfo, _ error) error {
		if info.IsDir() {
			return nil // descend
		}
		switch filepath.Ext(path) {
		case ".json":
			metadata := ParseJSON(Read(path))
			s.Add(filepath.Dir(path), metadata)
			Debugf("%s gathered (%d element(s))", path, len(metadata))
		}
		return nil
	}
}

func GatherSource(s StackReadWriter, m map[string]interface{}) filepath.WalkFunc {
	Debugf("gathering source")
	return func(path string, info os.FileInfo, _ error) error {
		if info.IsDir() {
			return nil // descend
		}
		switch filepath.Ext(path) {
		case ".html":
			defaultMetadata := map[string]interface{}{
				"source":  path,
				"target":  TargetFileFor(path, filepath.Ext(path)),
				"url":     "/" + Relative(*targetDir, TargetFileFor(path, filepath.Ext(path))),
				"sortkey": filepath.Base(path),
			}
			fileMetadata := map[string]interface{}{}
			fileMetadataBuf, _ := splitMetadata(Read(path))
			if len(fileMetadataBuf) > 0 {
				fileMetadata = ParseJSON(fileMetadataBuf)
			}
			inheritedMetadata := s.Get(path)
			metadata := mergemap.Merge(defaultMetadata, mergemap.Merge(inheritedMetadata, fileMetadata))
			s.Add(path, metadata)
			SplatInto(m, Relative(*sourceDir, path), metadata)
			Debugf("%s gathered (%d element(s))", path, len(metadata))

		case ".md":
			defaultMetadata := map[string]interface{}{
				"source":  path,
				"target":  TargetFileFor(path, ".html"),
				"url":     "/" + Relative(*targetDir, TargetFileFor(path, ".html")),
				"sortkey": filepath.Base(path),
			}
			if blogTuple, ok := NewBlogTuple(path, ".html"); ok {
				baseDir := filepath.Join(*targetDir, Relative(*sourceDir, filepath.Dir(path)))
				defaultMetadata["title"] = blogTuple.Title
				defaultMetadata["date"] = blogTuple.DateString()
				defaultMetadata["target"] = blogTuple.TargetFileFor(baseDir)
				defaultMetadata["url"] = "/" + Relative(*targetDir, blogTuple.TargetFileFor(baseDir))
				defaultMetadata["redirects"] = blogTuple.RedirectFromURLs(baseDir)
			}
			fileMetadata := map[string]interface{}{}
			fileMetadataBuf, _ := splitMetadata(Read(path))
			if len(fileMetadataBuf) > 0 {
				fileMetadata = ParseJSON(fileMetadataBuf)
			}
			inheritedMetadata := s.Get(path)
			metadata := mergemap.Merge(defaultMetadata, mergemap.Merge(inheritedMetadata, fileMetadata))
			s.Add(path, metadata)
			SplatInto(m, Relative(*sourceDir, path), metadata)
			Debugf("%s gathered (%d element(s))", path, len(metadata))
		}
		return nil
	}
}

func Transform(s StackReader) filepath.WalkFunc {
	Debugf("transforming")
	return func(path string, info os.FileInfo, _ error) error {
		if strings.HasPrefix(filepath.Base(path), ".") {
			Debugf("skip hidden file %s", path)
			return nil
		}
		if info.IsDir() {
			Debugf("descending into %s", path)
			return nil // descend
		}

		Debugf("Transforming %s", path)
		switch filepath.Ext(path) {
		case ".json":
			Debugf("%s ignored for transformation", path)

		case ".html":
			// read
			_, contentBuf := splitMetadata(Read(path))

			// render
			outputBuf := RenderTemplate(path, contentBuf, s.Get(path))

			// write
			dst := TargetFileFor(path, filepath.Ext(path))
			Write(dst, outputBuf)
			Debugf("%s transformed to %s", path, dst)

		case ".md":
			// read
			_, contentBuf := splitMetadata(Read(path))

			// render
			var htmlBits, extensionBits int
			metadata := s.Get(path)
			if v, ok := metadata["toc"]; ok && v.(bool) {
				htmlBits |= blackfriday.HTML_TOC
			}
			md := RenderTemplate(path, contentBuf, metadata)
			metadata = mergemap.Merge(metadata, map[string]interface{}{
				"content": template.HTML(RenderMarkdown(md, htmlBits, extensionBits)),
			})
			templatePath, templateBuf := Template(s, path)
			outputBuf := RenderTemplate(templatePath, templateBuf, metadata)

			// write file
			dst, _ := metadata["target"].(string)
			Write(dst, outputBuf)

			// write redirects
			if redirectsInterface, ok := metadata["redirects"]; ok {
				redirectToUrl, _ := metadata["url"].(string)
				redirectFromUrls, _ := redirectsInterface.([]string)
				for _, redirectFromUrl := range redirectFromUrls {
					redirectFromFile := filepath.Join(*targetDir, redirectFromUrl)
					Write(redirectFromFile, RedirectTo(redirectToUrl))
				}
			}

			// done
			Debugf("%s transformed to %s", path, dst)

		case ".source", ".template":
			Debugf("%s ignored for transformation", path)

		default:
			dst := TargetFileFor(path, filepath.Ext(path))
			Copy(dst, path)
			Debugf("%s transformed to %s verbatim", path, dst)
		}
		return nil
	}
}

func RenderTemplate(path string, input []byte, metadata map[string]interface{}) []byte {
	R := func(relativeFilename string) string {
		filename := filepath.Join(filepath.Dir(path), relativeFilename)
		return string(RenderTemplate(filename, Read(filename), metadata))
	}
	importhtml := func(relativeFilename string) template.HTML {
		return template.HTML(R(relativeFilename))
	}
	importcss := func(relativeFilename string) template.CSS {
		return template.CSS(R(relativeFilename))
	}
	importjs := func(relativeFilename string) template.JS {
		return template.JS(R(relativeFilename))
	}

	templateName := Relative(*sourceDir, path)
	funcMap := template.FuncMap{
		"importhtml": importhtml,
		"importcss":  importcss,
		"importjs":   importjs,
		"sorted":     SortedValues,
		"relative": func(s string) string {
			return Relative(filepath.Dir(metadata["url"].(string)), s)
		},
	}

	tmpl, err := template.New(templateName).Funcs(funcMap).Parse(string(input))
	if err != nil {
		Fatalf("Render Template %s: Parse: %s", path, err)
	}

	output := bytes.Buffer{}
	if err = tmpl.Execute(&output, metadata); err != nil {
		Fatalf("Render Template %s: Execute: %s", path, err)
	}

	return output.Bytes()
}

func RenderMarkdown(input []byte, htmlBits, extensionBits int) []byte {
	Debugf("rendering %d byte(s) of Markdown", len(input))

	htmlOptions := htmlBits // default
	htmlOptions |= blackfriday.HTML_USE_SMARTYPANTS
	title, css := "", ""
	htmlRenderer := blackfriday.HtmlRenderer(htmlOptions, title, css)

	extensions := extensionBits // default
	extensions |= blackfriday.EXTENSION_NO_INTRA_EMPHASIS
	extensions |= blackfriday.EXTENSION_TABLES
	extensions |= blackfriday.EXTENSION_FENCED_CODE
	extensions |= blackfriday.EXTENSION_AUTOLINK
	extensions |= blackfriday.EXTENSION_STRIKETHROUGH
	extensions |= blackfriday.EXTENSION_SPACE_HEADERS
	extensions |= blackfriday.EXTENSION_FOOTNOTES
	extensions |= blackfriday.EXTENSION_LAX_HTML_BLOCKS
	extensions |= blackfriday.EXTENSION_HEADER_IDS
	extensions |= blackfriday.EXTENSION_AUTO_HEADER_IDS

	return blackfriday.Markdown(input, htmlRenderer, extensions)
}
