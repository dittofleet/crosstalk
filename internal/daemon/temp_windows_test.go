package daemon

// shortTemp is where a test's socket goes. Windows has no /tmp, and its
// usual temp directory, which the empty string means, is short enough.
const shortTemp = ""
