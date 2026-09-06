package logging

import (
	"context"
	"log/slog"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"time"

	"the8020/kernel/identity"
	"the8020/kernel/logging/records"
)

type logEmitter interface {
	allows(string) bool
	emit(records.Record) bool
}

type handler struct {
	node    string
	emitter logEmitter
	attrs   []slog.Attr
	group   string
}

func (h *handler) Enabled(_ context.Context, level slog.Level) bool {
	return h.emitter.allows(severity(level))
}
func severity(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return "ERROR"
	case level >= slog.LevelWarn:
		return "WARN"
	case level >= slog.LevelInfo:
		return "INFO"
	default:
		return "DEBUG"
	}
}

func (h *handler) Handle(_ context.Context, entry slog.Record) error {
	r := records.Record{Time: entry.Time, Level: severity(entry.Level), Source: "kernel", Component: "kernel", NodeID: h.node, Message: entry.Message}
	if r.Time.IsZero() {
		r.Time = time.Now().UTC()
	}
	if fn := runtime.FuncForPC(entry.PC); fn != nil {
		path := strings.TrimPrefix(fn.Name(), "the8020/kernel/")
		if dot := strings.IndexByte(path, '.'); dot > 0 {
			r.Component = records.Text(path[:dot], 64)
		}
	}
	for _, a := range h.attrs {
		addAttr(&r, a, "", 0)
	}
	visited := 0
	entry.Attrs(func(a slog.Attr) bool { addAttr(&r, a, h.group, 0); visited++; return visited < 64 })
	h.emitter.emit(r)
	return nil
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	copy := *h
	copy.attrs = append([]slog.Attr(nil), h.attrs...)
	visited := 0
	copy.attrs = boundAttrs(copy.attrs, attrs, h.group, 0, &visited)
	return &copy
}

func boundAttrs(dst, attrs []slog.Attr, group string, depth int, visited *int) []slog.Attr {
	for _, a := range attrs {
		if len(dst) >= 32 || *visited >= 64 || depth >= 4 {
			break
		}
		*visited++
		if a.Value.Kind() == slog.KindGroup && !sensitive(a.Key) {
			prefix := group
			if a.Key != "" {
				prefix = records.Text(group+a.Key+".", 64)
			}
			dst = boundAttrs(dst, a.Value.Group(), prefix, depth+1, visited)
			continue
		}
		if a.Key == "" {
			continue
		}
		value := "[redacted]"
		if !sensitive(a.Key) {
			value = attrText(a.Value, 512, 0)
		}
		dst = append(dst, slog.String(records.Text(group+a.Key, 64), value))
	}
	return dst
}
func (h *handler) WithGroup(name string) slog.Handler {
	copy := *h
	if name != "" {
		copy.group = records.Text(h.group+name+".", 64)
	}
	return &copy
}

func sensitive(key string) bool {
	key = strings.ToLower(key)
	if dot := strings.LastIndexByte(key, '.'); dot >= 0 {
		key = key[dot+1:]
	}
	return key == "authorization" || key == "cookie" || key == "cookies" || key == "credentials" || key == "secrets" || key == "secret" || key == "secure_inputs" || key == "private_key" || strings.HasSuffix(key, "token") || strings.HasSuffix(key, "password")
}

func addAttr(r *records.Record, a slog.Attr, group string, depth int) {
	if a.Key == "" && a.Value.Kind() != slog.KindGroup {
		return
	}
	if a.Value.Kind() == slog.KindGroup && !sensitive(a.Key) {
		if depth >= 4 {
			return
		}
		prefix := group
		if a.Key != "" {
			prefix += records.Text(a.Key, 64) + "."
		}
		for i, child := range a.Value.Group() {
			if i >= 16 {
				break
			}
			addAttr(r, child, prefix, depth+1)
		}
		return
	}
	key := records.Text(group+a.Key, 64)
	value := "[redacted]"
	if !sensitive(a.Key) {
		value = attrText(a.Value, 512, 0)
	}
	if group == "" && a.Value.Kind() == slog.KindString {
		v := a.Value.String()
		switch a.Key {
		case "username":
			r.Username = v
			return
		case "component":
			r.Component = records.Text(v, 64)
			return
		case "node_id":
			if v == r.NodeID {
				return
			}
		case "sandbox_id":
			if identity.Is(v, "sbx") {
				r.SandboxID = v
				return
			}
		case "worker_id":
			if identity.Is(v, "wrk") {
				r.WorkerID = v
				return
			}
		case "context_id":
			if identity.Is(v, "ctx") {
				r.ContextID = v
				return
			}
		case "parent_context_id":
			if identity.Is(v, "ctx") {
				r.ParentContextID = v
				return
			}
		case "job_id", "execution_id":
			if identity.Is(v, "job") {
				r.JobID = v
				return
			}
		case "service_id":
			if identity.Is(v, "srv") {
				r.ServiceID = v
				return
			}
		case "persistent_execution_id":
			if identity.Is(v, "pex") {
				r.PersistentID = v
				return
			}
		case "object":
			r.Object = records.Text(v, 512)
			return
		case "program_id":
			r.Object = "program:" + records.Text(v, 512)
			return
		case "logical_service_id":
			r.Object = "service:" + records.Text(v, 512)
			return
		}
	}
	if len(r.Attributes) < 16 {
		if r.Attributes == nil {
			r.Attributes = make(map[string]string)
		}
		r.Attributes[key] = value
	}
}

func attrText(v slog.Value, budget, depth int) (text string) {
	defer func() {
		if recover() != nil {
			text = "[format failed]"
		}
	}()
	switch v.Kind() {
	case slog.KindString:
		return records.Text(v.String(), budget)
	case slog.KindBool:
		return strconv.FormatBool(v.Bool())
	case slog.KindInt64:
		return strconv.FormatInt(v.Int64(), 10)
	case slog.KindUint64:
		return strconv.FormatUint(v.Uint64(), 10)
	case slog.KindFloat64:
		return strconv.FormatFloat(v.Float64(), 'g', -1, 64)
	case slog.KindTime:
		return v.Time().UTC().Format(time.RFC3339Nano)
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindLogValuer:
		return "[LogValuer omitted]"
	default:
		return boundedValue(reflect.ValueOf(v.Any()), budget, depth)
	}
}

func boundedValue(value reflect.Value, budget, depth int) string {
	if !value.IsValid() {
		return "null"
	}
	if budget < 32 || depth >= 4 {
		return "…[value omitted]…"
	}
	if (value.Kind() == reflect.Pointer || value.Kind() == reflect.Interface) && value.IsNil() {
		return "null"
	}
	if value.CanInterface() {
		if err, ok := value.Interface().(error); ok {
			return records.Text(err.Error(), budget)
		}
	}
	switch value.Kind() {
	case reflect.Interface, reflect.Pointer:
		if value.IsNil() {
			return "null"
		}
		return boundedValue(value.Elem(), budget, depth+1)
	case reflect.String:
		return records.Text(value.String(), budget)
	case reflect.Bool:
		return strconv.FormatBool(value.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(value.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(value.Uint(), 10)
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(value.Float(), 'g', -1, 64)
	}
	var out strings.Builder
	appendValue := func(key string, v reflect.Value) {
		if out.Len() > 0 {
			out.WriteString(", ")
		}
		if key != "" {
			out.WriteString(records.Text(key, 48))
			out.WriteByte(':')
		}
		if sensitive(key) {
			out.WriteString("[redacted]")
		} else {
			out.WriteString(boundedValue(v, min(128, budget-out.Len()), depth+1))
		}
	}
	switch value.Kind() {
	case reflect.Map:
		iter := value.MapRange()
		for n := 0; n < 8 && out.Len() < budget-32 && iter.Next(); n++ {
			key := "key"
			if iter.Key().Kind() == reflect.String {
				key = iter.Key().String()
			}
			appendValue(key, iter.Value())
		}
		if value.Len() > 8 {
			out.WriteString(", …[fields omitted]…")
		}
	case reflect.Struct:
		for i, n := 0, 0; i < value.NumField() && n < 8 && out.Len() < budget-32; i++ {
			field := value.Type().Field(i)
			if field.PkgPath != "" {
				continue
			}
			appendValue(field.Name, value.Field(i))
			n++
		}
	case reflect.Array, reflect.Slice:
		n := value.Len()
		for i := 0; i < min(n, 8) && out.Len() < budget-32; i++ {
			index := i
			if n > 8 && i >= 4 {
				index = n - 8 + i
			}
			if i == 4 && n > 8 {
				out.WriteString(", …[items omitted]…")
			}
			appendValue("", value.Index(index))
		}
	default:
		return "[" + value.Kind().String() + "]"
	}
	return records.Text("{"+out.String()+"}", budget)
}
