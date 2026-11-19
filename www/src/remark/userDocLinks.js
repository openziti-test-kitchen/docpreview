// Rewrites links from the design documents into the user documentation.
//
// docs/design links to the user docs the way a reader browsing the repository on GitHub needs them:
// `../../www/docs/exposers.md`. Docusaurus resolves a relative .md link only inside the docs instance
// that holds the file, so from the design instance every one of those is a broken link, and
// onBrokenLinks fails the build over it.
//
// Editing the design documents would break them on GitHub instead. So this turns each one into the
// route the user docs publish it at: `../../www/docs/exposers.md#x` becomes `/docs/exposers#x`. An
// absolute route is resolved against baseUrl by Docusaurus's Link, so it survives a preview prefix.
//
// Registered as a beforeDefaultRemarkPlugin, so it runs before the .md link resolver warns about the
// original form.
const PREFIX = /^(?:\.\.\/)+www\/docs\/(.+?)\.mdx?(#.*)?$/;

function rewrite(node) {
  if (node.type === 'link' || node.type === 'definition') {
    const m = PREFIX.exec(node.url);
    if (m) {
      node.url = `/docs/${m[1]}${m[2] ?? ''}`;
    }
  }
  if (node.children) {
    node.children.forEach(rewrite);
  }
}

module.exports = function userDocLinks() {
  return rewrite;
};
