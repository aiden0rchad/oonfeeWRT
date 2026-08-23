package api

const (
	controllerPortableDatabaseMaxBytes = int64(1 << 30)
	controllerPortableArtifactMaxBytes = controllerPortableDatabaseMaxBytes + 16<<20
)
