/**
 * Disables the navbar search box.
 *
 * theme-classic renders `@theme/SearchBar` unconditionally and ships a stub that
 * returns null when no search plugin is configured. @netfoundry/docusaurus-theme
 * overrides that stub with a real Algolia DocSearch modal which, absent a
 * `themeConfig.algolia` block, falls back to NetFoundry's own appId/index. On
 * this site that produced a search box whose every result navigated away to
 * netfoundry.io — worse than no search at all.
 *
 * A site-local override wins over one from a theme plugin, so this puts the
 * stub back. Delete this file the day docpreview's docs get their own crawler
 * and a `themeConfig.algolia` block to point at it.
 */
export default function SearchBar(): null {
  return null;
}
