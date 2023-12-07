package inmemory

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	storagedriver "github.com/distribution/distribution/v3/registry/storage/driver"
	"github.com/distribution/distribution/v3/registry/storage/driver/base"
	"github.com/distribution/distribution/v3/registry/storage/driver/factory"
	"github.com/distribution/distribution/v3/tracing"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const driverName = "inmemory"

func init() {
	factory.Register(driverName, &inMemoryDriverFactory{})
}

// inMemoryDriverFacotry implements the factory.StorageDriverFactory interface.
type inMemoryDriverFactory struct{}

func (factory *inMemoryDriverFactory) Create(ctx context.Context, parameters map[string]interface{}) (storagedriver.StorageDriver, error) {
	return New(), nil
}

type driver struct {
	root  *dir
	mutex sync.RWMutex
}

// baseEmbed allows us to hide the Base embed.
type baseEmbed struct {
	base.Base
}

// Driver is a storagedriver.StorageDriver implementation backed by a local map.
// Intended solely for example and testing purposes.
type Driver struct {
	baseEmbed // embedded, hidden base driver.
}

var _ storagedriver.StorageDriver = &Driver{}

// New constructs a new Driver.
func New() *Driver {
	return &Driver{
		baseEmbed: baseEmbed{
			Base: base.Base{
				StorageDriver: &driver{
					root: &dir{
						common: common{
							p:   "/",
							mod: time.Now(),
						},
					},
				},
			},
		},
	}
}

// Implement the storagedriver.StorageDriver interface.

func (d *driver) Name() string {
	return driverName
}

// GetContent retrieves the content stored at "path" as a []byte.
func (d *driver) GetContent(ctx context.Context, path string) ([]byte, error) {
	span, spanCtx := tracing.StartSpan(ctx, fmt.Sprintf("%s:%s", driverName, "GetContent"),
		trace.WithAttributes(attribute.String("Path", path)))
	defer tracing.StopSpan(span)

	d.mutex.RLock()
	defer d.mutex.RUnlock()

	rc, err := d.reader(spanCtx, path, 0)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	return io.ReadAll(rc)
}

// PutContent stores the []byte content at a location designated by "path".
func (d *driver) PutContent(ctx context.Context, p string, contents []byte) error {
	span, _ := tracing.StartSpan(ctx, fmt.Sprintf("%s:%s", driverName, "PutContent"),
		trace.WithAttributes(attribute.String("subPath", p),
			attribute.Int("contentsLen", len(contents))))
	defer tracing.StopSpan(span)

	d.mutex.Lock()
	defer d.mutex.Unlock()

	normalized := normalize(p)

	f, err := d.root.mkfile(normalized)
	if err != nil {
		// TODO(stevvooe): Again, we need to clarify when this is not a
		// directory in StorageDriver API.
		return fmt.Errorf("not a file")
	}

	f.truncate()
	if _, err := f.WriteAt(contents, 0); err != nil {
		return err
	}

	return nil
}

// Reader retrieves an io.ReadCloser for the content stored at "path" with a
// given byte offset.
func (d *driver) Reader(ctx context.Context, path string, offset int64) (io.ReadCloser, error) {
	span, _ := tracing.StartSpan(ctx, fmt.Sprintf("%s:%s", driverName, "Reader"),
		trace.WithAttributes(attribute.String("path", path),
			attribute.Int64("offset", offset)))
	defer tracing.StopSpan(span)

	d.mutex.RLock()
	defer d.mutex.RUnlock()

	return d.reader(ctx, path, offset)
}

func (d *driver) reader(ctx context.Context, path string, offset int64) (io.ReadCloser, error) {
	if offset < 0 {
		return nil, storagedriver.InvalidOffsetError{Path: path, Offset: offset}
	}

	normalized := normalize(path)
	found := d.root.find(normalized)

	if found.path() != normalized {
		return nil, storagedriver.PathNotFoundError{Path: path}
	}

	if found.isdir() {
		return nil, fmt.Errorf("%q is a directory", path)
	}

	return io.NopCloser(found.(*file).sectionReader(offset)), nil
}

// Writer returns a FileWriter which will store the content written to it
// at the location designated by "path" after the call to Commit.
func (d *driver) Writer(ctx context.Context, path string, append bool) (storagedriver.FileWriter, error) {
	span, _ := tracing.StartSpan(ctx, fmt.Sprintf("%s:%s", driverName, "Writer"),
		trace.WithAttributes(attribute.String("path", path),
			attribute.Bool("append", append)))
	defer tracing.StopSpan(span)

	d.mutex.Lock()
	defer d.mutex.Unlock()

	normalized := normalize(path)

	f, err := d.root.mkfile(normalized)
	if err != nil {
		return nil, fmt.Errorf("not a file")
	}

	if !append {
		f.truncate()
	}

	return d.newWriter(f), nil
}

// Stat returns info about the provided path.
func (d *driver) Stat(ctx context.Context, path string) (storagedriver.FileInfo, error) {
	span, _ := tracing.StartSpan(ctx, fmt.Sprintf("%s:%s", driverName, "Stat"),
		trace.WithAttributes(attribute.String("path", path)))
	defer tracing.StopSpan(span)

	d.mutex.RLock()
	defer d.mutex.RUnlock()

	normalized := normalize(path)
	found := d.root.find(normalized)

	if found.path() != normalized {
		return nil, storagedriver.PathNotFoundError{Path: path}
	}

	fi := storagedriver.FileInfoFields{
		Path:    path,
		IsDir:   found.isdir(),
		ModTime: found.modtime(),
	}

	if !fi.IsDir {
		fi.Size = int64(len(found.(*file).data))
	}

	return storagedriver.FileInfoInternal{FileInfoFields: fi}, nil
}

// List returns a list of the objects that are direct descendants of the given
// path.
func (d *driver) List(ctx context.Context, path string) ([]string, error) {
	span, _ := tracing.StartSpan(ctx, fmt.Sprintf("%s:%s", driverName, "List"),
		trace.WithAttributes(attribute.String("path", path)))
	defer tracing.StopSpan(span)

	d.mutex.RLock()
	defer d.mutex.RUnlock()

	normalized := normalize(path)

	found := d.root.find(normalized)

	if !found.isdir() {
		return nil, fmt.Errorf("not a directory") // TODO(stevvooe): Need error type for this...
	}

	entries, err := found.(*dir).list(normalized)
	if err != nil {
		switch err {
		case errNotExists:
			return nil, storagedriver.PathNotFoundError{Path: path}
		case errIsNotDir:
			return nil, fmt.Errorf("not a directory")
		default:
			return nil, err
		}
	}

	return entries, nil
}

// Move moves an object stored at sourcePath to destPath, removing the original
// object.
func (d *driver) Move(ctx context.Context, sourcePath string, destPath string) error {
	span, _ := tracing.StartSpan(ctx, fmt.Sprintf("%s:%s", driverName, "Move"),
		trace.WithAttributes(attribute.String("sourcePath", sourcePath),
			attribute.String("destPath", destPath)))
	defer tracing.StopSpan(span)

	d.mutex.Lock()
	defer d.mutex.Unlock()

	normalizedSrc, normalizedDst := normalize(sourcePath), normalize(destPath)

	err := d.root.move(normalizedSrc, normalizedDst)
	switch err {
	case errNotExists:
		return storagedriver.PathNotFoundError{Path: destPath}
	default:
		return err
	}
}

// Delete recursively deletes all objects stored at "path" and its subpaths.
func (d *driver) Delete(ctx context.Context, path string) error {
	span, _ := tracing.StartSpan(ctx, fmt.Sprintf("%s:%s", driverName, "Delete"),
		trace.WithAttributes(attribute.String("path", path)))
	defer tracing.StopSpan(span)

	d.mutex.Lock()
	defer d.mutex.Unlock()

	normalized := normalize(path)

	err := d.root.delete(normalized)
	switch err {
	case errNotExists:
		return storagedriver.PathNotFoundError{Path: path}
	default:
		return err
	}
}

// RedirectURL returns a URL which may be used to retrieve the content stored at the given path.
func (d *driver) RedirectURL(*http.Request, string) (string, error) {
	return "", nil
}

// Walk traverses a filesystem defined within driver, starting
// from the given path, calling f on each file and directory
func (d *driver) Walk(ctx context.Context, path string, f storagedriver.WalkFn, options ...func(*storagedriver.WalkOptions)) error {
	span, spanCtx := tracing.StartSpan(ctx, fmt.Sprintf("%s:%s", driverName, "Walk"),
		trace.WithAttributes(attribute.String("path", path)))
	defer tracing.StopSpan(span)

	return storagedriver.WalkFallback(spanCtx, d, path, f, options...)
}

type writer struct {
	d         *driver
	f         *file
	buffer    []byte
	buffSize  int
	closed    bool
	committed bool
	cancelled bool
}

func (d *driver) newWriter(f *file) storagedriver.FileWriter {
	return &writer{
		d: d,
		f: f,
	}
}

func (w *writer) Write(p []byte) (int, error) {
	if w.closed {
		return 0, fmt.Errorf("already closed")
	} else if w.committed {
		return 0, fmt.Errorf("already committed")
	} else if w.cancelled {
		return 0, fmt.Errorf("already cancelled")
	}

	w.d.mutex.Lock()
	defer w.d.mutex.Unlock()
	if cap(w.buffer) < len(p)+w.buffSize {
		data := make([]byte, len(w.buffer), len(p)+w.buffSize)
		copy(data, w.buffer)
		w.buffer = data
	}

	w.buffer = w.buffer[:w.buffSize+len(p)]
	n := copy(w.buffer[w.buffSize:w.buffSize+len(p)], p)
	w.buffSize += n

	return n, nil
}

func (w *writer) Size() int64 {
	w.d.mutex.RLock()
	defer w.d.mutex.RUnlock()

	return int64(len(w.f.data))
}

func (w *writer) Close() error {
	if w.closed {
		return fmt.Errorf("already closed")
	}
	w.closed = true

	if err := w.flush(); err != nil {
		return err
	}

	return nil
}

func (w *writer) Cancel(ctx context.Context) error {
	if w.closed {
		return fmt.Errorf("already closed")
	} else if w.committed {
		return fmt.Errorf("already committed")
	}
	w.cancelled = true

	w.d.mutex.Lock()
	defer w.d.mutex.Unlock()

	return w.d.root.delete(w.f.path())
}

func (w *writer) Commit(ctx context.Context) error {
	if w.closed {
		return fmt.Errorf("already closed")
	} else if w.committed {
		return fmt.Errorf("already committed")
	} else if w.cancelled {
		return fmt.Errorf("already cancelled")
	}
	w.committed = true

	if err := w.flush(); err != nil {
		return err
	}

	return nil
}

func (w *writer) flush() error {
	w.d.mutex.Lock()
	defer w.d.mutex.Unlock()

	if _, err := w.f.WriteAt(w.buffer, int64(len(w.f.data))); err != nil {
		return err
	}
	w.buffer = []byte{}
	w.buffSize = 0

	return nil
}
