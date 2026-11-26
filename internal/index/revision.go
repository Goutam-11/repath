package index

// BuildRevision names the extraction and resolution code in this binary.
//
// The commit cache and the active index are trusted only when the revision
// they were built with matches this value: a repath upgrade can change the
// graph without changing store.SchemaVersion (issue #134). The build injects
// the binary version; tests set it directly.
var BuildRevision = "dev"
