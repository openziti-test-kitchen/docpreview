// Self-hosted fonts, loaded as a client module.
//
// Imported from JavaScript rather than with an @import in custom.css. Through the @import, the font files'
// url()s came out root-relative (/assets/fonts/...) with no baseUrl, so on GitHub Pages, served under
// /docpreview/, every font 404'd and the site fell back to system fonts. Through webpack they get the
// public path, which is the baseUrl.
import '@fontsource-variable/inter';
import '@fontsource-variable/jetbrains-mono';
