package variant

import (
	"fmt"
	"os"
	"path"
	"strings"

	log "github.com/Sirupsen/logrus"
	"github.com/juju/errors"
	bunyan "github.com/mumoshu/logrus-bunyan-formatter"
	"github.com/spf13/viper"

	"encoding/json"
	"github.com/mumoshu/variant/pkg/api/task"
	"github.com/mumoshu/variant/pkg/util/maputil"
	"github.com/xeipuuv/gojsonschema"
	"reflect"
	"strconv"
)

type Application struct {
	Name                string
	CommandRelativePath string
	CachedTaskOutputs   map[string]interface{}
	ConfigFile          string
	Verbose             bool
	Output              string
	Env                 string
	TaskRegistry        *TaskRegistry
	InputResolver       InputResolver
	TaskNamer           *TaskNamer
	LogToStderr         bool
}

func (p Application) UpdateLoggingConfiguration() {
	if p.Verbose {
		log.SetLevel(log.DebugLevel)
	}

	if p.LogToStderr {
		log.SetOutput(os.Stderr)
	}

	commandName := path.Base(os.Args[0])
	if p.Output == "bunyan" {
		log.SetFormatter(&bunyan.Formatter{Name: commandName})
	} else if p.Output == "json" {
		log.SetFormatter(&log.JSONFormatter{})
	} else if p.Output == "text" {
		log.SetFormatter(&log.TextFormatter{})
	} else if p.Output == "message" {
		log.SetFormatter(&MessageOnlyFormatter{})
	} else {
		log.Fatalf("Unexpected output format specified: %s", p.Output)
	}
}

func (p Application) RunTaskForKeyString(keyStr string, args []string, arguments task.Arguments, scope map[string]interface{}, caller ...*Task) (string, error) {
	taskKey := p.TaskNamer.FromString(fmt.Sprintf("%s.%s", p.Name, keyStr))
	return p.RunTask(taskKey, args, arguments, scope, caller...)
}

func (p Application) RunTask(taskName TaskName, args []string, arguments task.Arguments, scope map[string]interface{}, caller ...*Task) (string, error) {
	var ctx *log.Entry

	if len(caller) == 1 {
		ctx = log.WithFields(log.Fields{"task": taskName.ShortString(), "caller": caller[0].GetKey().ShortString()})
	} else {
		ctx = log.WithFields(log.Fields{"task": taskName.ShortString()})
	}

	ctx.Debugf("app started task %s", taskName.ShortString())

	provided := p.GetTmplOrTypedValueForConfigKey(taskName.ShortString(), "string")

	if provided != nil {
		p := fmt.Sprintf("%v", provided)
		ctx.Debugf("app skipped task %s via provided value: %s", taskName.ShortString(), p)
		ctx.Info(p)
		println(p)
		return p, nil
	}

	taskDef, err := p.TaskRegistry.FindTask(taskName)

	if err != nil {
		return "", errors.Annotatef(err, "app failed finding task %s", taskName.ShortString())
	}

	vars := map[string](interface{}){}
	vars["args"] = args
	vars["env"] = p.Env
	vars["cmd"] = p.CommandRelativePath

	inputs, err := p.InheritedInputValuesForTaskKey(taskName, args, arguments, scope, caller...)

	if err != nil {
		return "", errors.Annotatef(err, "app failed running task %s", taskName.ShortString())
	}

	for k, v := range inputs {
		vars[k] = v
	}

	kv := maputil.Flatten(vars)

	s, err := jsonschemaFromInputs(taskDef.Inputs)
	if err != nil {
		return "", errors.Annotatef(err, "app failed while generating jsonschema from: %v", taskDef.Inputs)
	}
	doc := gojsonschema.NewGoLoader(kv)
	result, err := s.Validate(doc)
	if result.Valid() {
		log.Debugf("all the inputs are valid")
	} else {
		log.Errorf("one or more inputs are not valid in %v:", kv)
		for _, err := range result.Errors() {
			// Err implements the ResultError interface
			log.Errorf("- %s", err)
		}
		return "", errors.Annotatef(err, "app failed validating inputs: %v", doc)
	}

	ctx.WithField("variables", kv).Debugf("app bound variables for task %s", taskName.ShortString())

	taskTemplate := NewTaskTemplate(taskDef, vars)
	taskRunner, err := NewTaskRunner(taskDef, taskTemplate, vars)
	if err != nil {
		return "", errors.Annotatef(err, "failed to initialize task runner")
	}

	output, error := taskRunner.Run(&p, caller...)

	ctx.Debugf("app received output from task %s: %s", taskName.ShortString(), output)

	if error != nil {
		error = errors.Annotatef(error, "app failed running task %s", taskName.ShortString())
	}

	ctx.Debugf("app finished running task %s", taskName.ShortString())

	return output, error
}

func (p Application) InheritedInputValuesForTaskKey(taskName TaskName, args []string, arguments task.Arguments, scope map[string]interface{}, caller ...*Task) (map[string]interface{}, error) {
	result := map[string]interface{}{}

	// TODO make this parents-first instead of children-first?
	direct, err := p.DirectInputValuesForTaskKey(taskName, args, arguments, scope, caller...)

	if err != nil {
		return nil, errors.Annotatef(err, "One or more inputs for task %s failed", taskName.ShortString())
	}

	for k, v := range direct {
		result[k] = v
	}

	parentKey, err := taskName.Parent()

	if err == nil {
		inherited, err := p.InheritedInputValuesForTaskKey(parentKey, []string{}, arguments, map[string]interface{}{}, caller...)

		if err != nil {
			return nil, errors.Annotatef(err, "AggregateInputsForParent(%s) failed", taskName.ShortString())
		}

		maputil.DeepMerge(result, inherited)
	}

	return result, nil
}

type AnyMap map[string]interface{}

func (p Application) GetTmplOrTypedValueForConfigKey(k string, tpe string) interface{} {
	ctx := log.WithFields(log.Fields{"key": k})

	lastIndex := strings.LastIndex(k, ".")

	ctx.Debugf("fetching %s for %s", k, tpe)

	flagKey := fmt.Sprintf("flags.%s", k)
	ctx.Debugf("fetching %s", flagKey)
	valueFromFlag := viper.Get(flagKey)
	if valueFromFlag != nil && valueFromFlag != "" {
		// From flags
		if s, ok := valueFromFlag.(string); ok {
			return s
		}
		// From configs
		if any, ok := ensureType(valueFromFlag, tpe); ok {
			return any
		}
		ctx.Debugf("found %v, but its type was %s(expected %s)", valueFromFlag, reflect.TypeOf(valueFromFlag), tpe)
	}

	ctx.Debugf("index: %d", lastIndex)

	if lastIndex != -1 {
		a := []rune(k)
		k1 := string(a[:lastIndex])
		k2 := string(a[lastIndex+1:])

		ctx.Debugf("viper.Get(%v): %v", k1, viper.Get(k1))

		if viper.Get(k1) != nil {

			values := viper.Sub(k1)

			ctx.Debugf("app fetched %s: %v", k1, values)

			var provided interface{}

			if values != nil && values.Get(k2) != nil {
				provided = values.Get(k2)
			} else {
				provided = nil
			}

			ctx.Debugf("app fetched %s[%s]: %s", k1, k2, provided)

			if provided != nil {
				return provided
			}
		}
		return nil
	} else {
		raw := viper.Get(k)
		ctx.Debugf("app fetched raw value for key %s: %v", k, raw)
		ctx.Debugf("type of value fetched: expected %s, got %v", tpe, reflect.TypeOf(raw))
		if raw == nil {
			return nil
		}

		// From flags
		if s, ok := raw.(string); ok {
			return s
		}

		// From configs
		v, ok := ensureType(raw, tpe)
		if ok {
			return v
		}

		ctx.Debugf("ignoring: unexpected type of value fetched: expected %s, but got %v", tpe, reflect.TypeOf(raw))
		return nil
	}
}

func ensureType(raw interface{}, tpe string) (interface{}, bool) {
	if tpe == "string" {
		if _, ok := raw.(string); ok {
			return raw, true
		}
		return nil, false
	}

	if tpe == "integer" {
		switch raw.(type) {
		case int, int64:
			return raw, true
		}
		return nil, false
	}

	if tpe == "bool" {
		switch raw.(type) {
		case bool:
			return raw, true
		}
		return nil, false
	}

	return nil, false
}

func (p Application) DirectInputValuesForTaskKey(taskName TaskName, args []string, arguments task.Arguments, scope map[string]interface{}, caller ...*Task) (map[string]interface{}, error) {
	var ctx *log.Entry

	if len(caller) == 1 {
		ctx = log.WithFields(log.Fields{"caller": caller[0].Name.ShortString(), "task": taskName.ShortString()})
	} else {
		ctx = log.WithFields(log.Fields{"task": taskName.ShortString()})
	}

	values := map[string]interface{}{}

	var baseTaskKey string
	if len(caller) > 0 {
		baseTaskKey = caller[0].GetKey().ShortString()
	} else {
		baseTaskKey = ""
	}

	ctx.Debugf("app started collecting inputs")

	currentTask, err := p.TaskRegistry.FindTask(taskName)
	if err != nil {
		return nil, errors.Trace(err)
	}

	for _, input := range currentTask.ResolvedInputs {
		ctx.Debugf("task %s depends on input %s", taskName, input.ShortName())

		var tmplOrStaticVal interface{}
		var value interface{}

		if i := input.ArgumentIndex; i != nil && len(args) >= *i+1 {
			ctx.Debugf("app found positional argument: args[%d]=%s", input.ArgumentIndex, args[*i])
			tmplOrStaticVal = args[*i]
		}

		if tmplOrStaticVal == nil {
			if v, err := arguments.Get(input.Name); err == nil {
				tmplOrStaticVal = v
			}
		}

		if tmplOrStaticVal == nil && input.Name != input.ShortName() {
			if v, err := arguments.Get(input.ShortName()); err == nil {
				tmplOrStaticVal = v
			}
		}

		if tmplOrStaticVal == nil && baseTaskKey != "" {
			tmplOrStaticVal = p.GetTmplOrTypedValueForConfigKey(fmt.Sprintf("%s.%s", baseTaskKey, input.ShortName()), input.TypeName())
		}

		if tmplOrStaticVal == nil && strings.LastIndex(input.ShortName(), taskName.ShortString()) == -1 {
			tmplOrStaticVal = p.GetTmplOrTypedValueForConfigKey(fmt.Sprintf("%s.%s", taskName.ShortString(), input.ShortName()), input.TypeName())
		}

		if tmplOrStaticVal == nil {
			tmplOrStaticVal = p.GetTmplOrTypedValueForConfigKey(input.ShortName(), input.TypeName())
		}

		if tmplOrStaticVal == nil {
			if input.Default != nil {
				switch input.TypeName() {
				case "string":
					value = input.DefaultAsString()
				case "integer":
					value = input.DefaultAsInt()
				case "boolean":
					value = input.DefaultAsBool()
				case "array":
					v, err := input.DefaultAsArray()
					if err != nil {
						return nil, errors.Annotatef(err, "failed to parse default value as array: %v", input.Default)
					}
					value = v
				case "object":
					v, err := input.DefaultAsObject()
					if err != nil {
						return nil, errors.Annotatef(err, "failed to parse default value as map: %v", input.Default)
					}
					value = v
				default:
					return nil, fmt.Errorf("unsupported input type `%s` found. the type should be one of: string, integer, boolean", input.TypeName())
				}
			} else if input.Name == "env" {
				value = ""
			}
		}

		pathComponents := strings.Split(input.Name, ".")

		if value == nil {
			// Missed all the value sources(default, args, params, options)
			if tmplOrStaticVal == nil {
				var output interface{}
				var err error
				if output, err = maputil.GetValueAtPath(p.CachedTaskOutputs, pathComponents); output == nil {
					args := arguments.GetSubOrEmpty(input.Name)
					tmplOrStaticVal, err = p.RunTask(p.TaskNamer.FromResolvedInput(input), []string{}, args, map[string]interface{}{}, currentTask)
					if err != nil {
						return nil, errors.Annotatef(err, "Missing value for input `%s`. Please provide a command line option or a positional argument or a task for it`", input.ShortName())
					}
					maputil.SetValueAtPath(p.CachedTaskOutputs, pathComponents, tmplOrStaticVal)
				} else {
					tmplOrStaticVal = output
				}
				if err != nil {
					return nil, errors.Trace(err)
				}
			}
			// Now that the tmplOrStaticVal exists, render add type it
			log.Debugf("tmplOrStaticVal=%v", tmplOrStaticVal)
			if tmplOrStaticVal != nil {
				var renderedValue string
				{
					expr, ok := tmplOrStaticVal.(string)
					if ok {
						taskTemplate := NewTaskTemplate(currentTask, scope)
						log.Debugf("rendering %s", expr)
						r, err := taskTemplate.Render(expr, input.Name)
						if err != nil {
							return nil, errors.Annotate(err, "failed to render task template")
						}
						renderedValue = r
						log.Debugf("converting type of %v", renderedValue)
						value, err = parseSupportedValueFromString(renderedValue, input.TypeName())
						if err != nil {
							return nil, err
						}
						log.Debugf("value after type conversion=%v", value)
					} else {
						value = tmplOrStaticVal
					}
				}
			} else {
				// the dependent task succeeded with no output
			}
		}

		maputil.SetValueAtPath(values, pathComponents, value)
	}

	ctx.WithField("values", values).Debugf("app finished collecting inputs")

	return values, nil
}

func parseSupportedValueFromString(renderedValue string, typeName string) (interface{}, error) {
	switch typeName {
	case "string":
		log.Debugf("string=%v", renderedValue)
		return renderedValue, nil
	case "integer":
		log.Debugf("integer=%v", renderedValue)
		value, err := strconv.Atoi(renderedValue)
		if err != nil {
			return nil, errors.Annotatef(err, "%v can't be casted to integer", renderedValue)
		}
		return value, nil
	case "boolean":
		log.Debugf("boolean=%v", renderedValue)
		switch renderedValue {
		case "true":
			return true, nil
		case "false":
			return false, nil
		default:
			return nil, fmt.Errorf("%v can't be parsed as boolean", renderedValue)
		}
	case "array", "object":
		log.Debugf("converting this to either array or object=%v", renderedValue)
		var dst interface{}
		if err := json.Unmarshal([]byte(renderedValue), &dst); err != nil {
			return nil, errors.Annotatef(err, "failed converting: failed to parse %s as json", renderedValue)
		}
		return dst, nil
	default:
		log.Debugf("foobar")
		return nil, fmt.Errorf("unsupported input type `%s` found. the type should be one of: string, integer, boolean", typeName)
	}
}

func (p *Application) Tasks() map[string]*Task {
	return p.TaskRegistry.Tasks()
}

func jsonschemaFromInputs(inputs []*InputConfig) (*gojsonschema.Schema, error) {
	required := []string{}
	props := map[string]map[string]interface{}{}
	for _, input := range inputs {
		name := strings.Replace(input.Name, "-", "_", -1)
		props[name] = input.JSONSchema()

		if input.Required() {
			required = append(required, name)
		}
	}
	goschema := map[string]interface{}{
		"type":       "object",
		"properties": props,
		"required":   required,
	}
	schemaLoader := gojsonschema.NewGoLoader(goschema)
	return gojsonschema.NewSchema(schemaLoader)
}
