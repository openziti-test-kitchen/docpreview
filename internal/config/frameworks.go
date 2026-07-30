package config

// Framework presets: what a documentation site of a given kind is built with.
//
// The idea is Vercel's "Framework Settings", and it earns its place for the same reason
// there: the commonest project is one where every field on the form has a right answer
// that nobody should have to look up. Picking "Docusaurus" is one decision that replaces
// two, and it replaces them with the values that repository would have used anyway.
//
// # What a preset does not include
//
// **No install command.** Vercel has that field; this does not, and the omission is
// deliberate. The install command is derived from the lockfile the repository committed —
// `pipeline.installCommand` — because the lockfile is the author's statement of which
// package manager owns the tree, and running the wrong one does not merely take longer:
// `npm ci` against a tree with only a `yarn.lock` fails outright, and `npm install` there
// resolves a different dependency graph than the one the author tested. A preset cannot
// know that; the tree can.
//
// **No development command.** Nothing here ever runs a dev server.
//
// # Precedence
//
// A project's own explicit field wins over its preset, and the preset wins over the
// repository's `.docpreview.yml`. The second half is worth stating because it is the
// surprising one: the repository's file is what it says it is, and an operator who chose a
// preset in the dashboard has said something more recent and more specific. Choosing the
// blank preset — "the repository decides" — is how to defer to the file, and it is the
// default, so nothing changes for a project nobody has touched.
type Framework struct {
	// ID is stored on the project row. Stable: renaming one orphans every project that
	// named it, which then silently falls back to the repository's own configuration.
	ID string `json:"id"`

	// Label is what the dropdown shows.
	Label string `json:"label"`

	// BuildCommand and Output are the preset's answers, both relative to the project's
	// build directory the same way the operator's own values are.
	BuildCommand string `json:"build_command,omitempty"`
	Output       string `json:"output,omitempty"`

	// Dir is where package.json lives, relative to the repository root. Empty means the
	// root, which is what most presets want.
	//
	// Set only where a convention is strong enough to be worth guessing at. A wrong
	// directory fails differently from a wrong build command — "no package.json in
	// docusaurus" rather than a command that runs and produces nothing — so a preset that
	// guesses one is making a claim about repository layout, not about a tool.
	Dir string `json:"dir,omitempty"`

	// NeedsTool names a program the container image must already have, for the presets
	// that are not Node. An operator picking MkDocs on `node:24-bookworm` gets a build
	// that fails with "mkdocs: not found", and the form can say so first.
	NeedsTool string `json:"needs_tool,omitempty"`
}

// FrameworkNone means no preset: the repository's own configuration decides. It stays the
// meaning of an empty stored value, so a project row written before presets existed keeps
// behaving exactly as it did.
const FrameworkNone = ""

// FrameworkDefault is what a *new* project's form starts on.
//
// Docusaurus rather than none, because this is a Docusaurus previewer in practice: every
// project on this daemon is one, the demo harness drives four of them, and the whole
// pipeline — the base URL check, the node images, the cached node_modules — is shaped
// around it. A form that starts on "the repository decides" makes the commonest case two
// clicks and the rarest case zero.
//
// Only the form's initial selection. It is not applied to a stored blank, which would
// change what every existing project builds.
const FrameworkDefault = "docusaurus"

// frameworks is the table, ordered as the dropdown shows it — the documentation
// generators first, since that is what this tool is for, then the general-purpose site
// builders that people also write docs in.
//
// Every entry was taken from that generator's own documented default output directory
// rather than from memory of a project that had customised it. Where a generator has no
// single answer, it is not listed: a wrong preset is worse than no preset, because it
// looks configured.
var frameworks = []Framework{
	{ID: FrameworkNone, Label: "None — the repository decides"},

	// The only preset that guesses a directory. Every Docusaurus repository this daemon
	// builds keeps the site in a subdirectory rather than at the root — `docusaurus/` in
	// customer-connect-docs, `unified-doc/` in docusaurus-shared — so the root is the one
	// answer that is reliably wrong here. A project whose site is elsewhere types the
	// directory, which is one field either way; a project at the root clears it.
	{ID: "docusaurus", Label: "Docusaurus (v2+)",
		BuildCommand: "npm run build", Output: "build", Dir: "docusaurus"},
	{ID: "vitepress", Label: "VitePress",
		BuildCommand: "npm run docs:build", Output: ".vitepress/dist"},
	{ID: "vuepress", Label: "VuePress",
		BuildCommand: "npm run docs:build", Output: ".vuepress/dist"},
	{ID: "starlight", Label: "Astro Starlight",
		BuildCommand: "npm run build", Output: "dist"},
	{ID: "mkdocs", Label: "MkDocs",
		BuildCommand: "mkdocs build", Output: "site", NeedsTool: "mkdocs"},
	{ID: "sphinx", Label: "Sphinx",
		BuildCommand: "sphinx-build -b html . _build/html", Output: "_build/html",
		NeedsTool: "sphinx-build"},
	{ID: "mdbook", Label: "mdBook",
		BuildCommand: "mdbook build", Output: "book", NeedsTool: "mdbook"},
	{ID: "antora", Label: "Antora",
		BuildCommand: "npx antora antora-playbook.yml", Output: "build/site"},
	{ID: "retype", Label: "Retype",
		BuildCommand: "npx retypeapp build", Output: ".retype"},

	{ID: "astro", Label: "Astro", BuildCommand: "npm run build", Output: "dist"},
	{ID: "eleventy", Label: "Eleventy", BuildCommand: "npx @11ty/eleventy", Output: "_site"},
	{ID: "hugo", Label: "Hugo", BuildCommand: "hugo --minify", Output: "public",
		NeedsTool: "hugo"},
	{ID: "jekyll", Label: "Jekyll", BuildCommand: "bundle exec jekyll build", Output: "_site",
		NeedsTool: "bundle"},
	{ID: "gatsby", Label: "Gatsby", BuildCommand: "npm run build", Output: "public"},
	{ID: "nextjs", Label: "Next.js (static export)",
		BuildCommand: "npm run build", Output: "out"},
	{ID: "nuxt", Label: "Nuxt (static)", BuildCommand: "npm run generate", Output: "dist"},
	{ID: "sveltekit", Label: "SvelteKit (static adapter)",
		BuildCommand: "npm run build", Output: "build"},
	{ID: "vite", Label: "Vite", BuildCommand: "npm run build", Output: "dist"},
}

// Frameworks returns the preset table.
//
// A copy, because the caller marshals it into a page and a caller that mutated the
// package's own slice would change what every later request sees.
func Frameworks() []Framework {
	out := make([]Framework, len(frameworks))
	copy(out, frameworks)
	return out
}

// FrameworkByID returns one preset and whether it exists.
//
// An unknown id is not an error anywhere: it means a project row names a preset this
// binary does not have — a downgrade, or a table entry removed — and the honest response
// is to fall back to the repository's own configuration rather than to refuse the build.
func FrameworkByID(id string) (Framework, bool) {
	if id == FrameworkNone {
		return Framework{}, false
	}
	for _, f := range frameworks {
		if f.ID == id {
			return f, true
		}
	}
	return Framework{}, false
}
