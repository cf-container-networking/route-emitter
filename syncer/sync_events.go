package syncer

type Events struct {
	Sync         chan struct{}
	Emit         chan struct{}
	InternalSync chan struct{}
	InternalEmit chan struct{}
}
