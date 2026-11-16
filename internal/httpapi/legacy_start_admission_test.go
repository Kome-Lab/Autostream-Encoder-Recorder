package httpapi

// These adapters keep existing v2 assertions on an explicitly unmanaged synthetic
// peer. Production constructors remain strict. IU-WS2-10 tests use them directly.
import (
	"context"
	lifecycle "github.com/Kome-Lab/Autostream-Contracts/v3lifecycle"
	"github.com/example/autostream-encoder-recorder/internal/manualhold"
	"github.com/example/autostream-encoder-recorder/internal/streamproc"
	"github.com/example/autostream-encoder-recorder/internal/videoingest"
	"github.com/example/autostream-encoder-recorder/internal/workerevents"
	"net/http"
	"os"
	"path/filepath"
)

func legacyStartManager(manager *streamproc.Manager) *streamproc.Manager {
	if manager == nil {
		return nil
	}
	guard, err := manualhold.New(filepath.Join(os.TempDir(), "autostream-explicit-legacy-unmanaged-v2"), manualhold.InspectorFunc(func(context.Context, lifecycle.Record) (lifecycle.Record, error) {
		return lifecycle.Record{}, manualhold.ErrUnknown
	}), func() (lifecycle.ProducerRef, error) { return lifecycle.ProducerRef{}, manualhold.ErrUnknown })
	if err != nil {
		panic(err)
	}
	guard.Require = func() bool { return false }
	manager.StartAdmission = guard
	return manager
}
func legacyNewServer(service string) http.Handler {
	return NewServerWithProcessManager(service, legacyStartManager(streamproc.NewManagerFromEnv()))
}
func legacyNewServerWithProcessManager(service string, manager *streamproc.Manager) http.Handler {
	return NewServerWithProcessManager(service, legacyStartManager(manager))
}
func legacyNewServerWithManagers(service string, manager *streamproc.Manager, events *workerevents.Manager, verifier TokenVerifier) http.Handler {
	return NewServerWithManagers(service, legacyStartManager(manager), events, verifier)
}
func legacyNewServerWithManagersAndSecretResolver(service string, manager *streamproc.Manager, events *workerevents.Manager, verifier TokenVerifier, resolver RuntimeSecretResolver) http.Handler {
	return NewServerWithManagersAndSecretResolver(service, legacyStartManager(manager), events, verifier, resolver)
}
func legacyNewServerWithManagersAndRuntimeConfig(service string, manager *streamproc.Manager, events *workerevents.Manager, verifier TokenVerifier, resolver RuntimeSecretResolver, runtime RuntimeConfigProvider) http.Handler {
	return NewServerWithManagersAndRuntimeConfig(service, legacyStartManager(manager), events, verifier, resolver, runtime)
}
func legacyNewServerWithManagersAndRuntimeConfigAndUpdaterIdentity(service string, manager *streamproc.Manager, events *workerevents.Manager, verifier TokenVerifier, resolver RuntimeSecretResolver, runtime RuntimeConfigProvider, updater *UpdaterIdentityLatch) http.Handler {
	return NewServerWithManagersAndRuntimeConfigAndUpdaterIdentity(service, legacyStartManager(manager), events, verifier, resolver, runtime, updater)
}
func legacynewServerWithManagersAndRuntimeConfigAndUpdaterIdentity(service string, manager *streamproc.Manager, events *workerevents.Manager, verifier TokenVerifier, resolver RuntimeSecretResolver, runtime RuntimeConfigProvider, updater *UpdaterIdentityLatch, video *videoingest.Manager) http.Handler {
	return newServerWithManagersAndRuntimeConfigAndUpdaterIdentity(service, legacyStartManager(manager), events, verifier, resolver, runtime, updater, video)
}
