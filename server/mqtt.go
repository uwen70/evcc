package server

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/cmd/shutdown"
	"github.com/evcc-io/evcc/core/loadpoint"
	"github.com/evcc-io/evcc/core/site"
	"github.com/evcc-io/evcc/core/vehicle"
	"github.com/evcc-io/evcc/plugin/mqtt"
	"github.com/evcc-io/evcc/util"
	"github.com/samber/lo"
)

// MQTTMarshaler is the interface implemented by types that
// can marshal themselves into valid an MQTT string representation.
type MQTTMarshaler interface {
	MarshalMQTT() ([]byte, error)
}

// MQTT is the MQTT server. It uses the MQTT client for publishing.
type MQTT struct {
	log       *util.Logger
	Handler   *mqtt.Client
	root      string
	publisher func(topic string, retained bool, payload string)
}

// NewMQTT creates MQTT server
func NewMQTT(root string, site site.API) (*MQTT, error) {
	m := &MQTT{
		log:     util.NewLogger("mqtt"),
		Handler: mqtt.Instance,
		root:    root,
	}
	m.publisher = m.publishString

	err := m.Handler.Cleanup(m.root, true)
	if err == nil {
		err = m.Listen(site)
	}
	if err != nil {
		err = fmt.Errorf("mqtt: %w", err)
	}

	shutdown.Register(func() {
		m.log.DEBUG.Println("shutdown cleanup started")
		if err := m.Handler.Cleanup(m.root, true); err != nil {
			m.log.ERROR.Printf("shutdown cleanup failed: %v", err)
		}
	})

	return m, err
}

func (m *MQTT) encode(v interface{}) string {
	// nil should erase the value
	if v == nil {
		return ""
	}

	switch val := v.(type) {
	case string:
		return val
	case float64:
		return fmt.Sprintf("%.5g", val)
	case time.Time:
		if val.IsZero() {
			return ""
		}
		return strconv.FormatInt(val.Unix(), 10)
	case time.Duration:
		// must be before stringer to convert to seconds instead of string
		return strconv.Itoa(int(val.Seconds()))
	case fmt.Stringer:
		return val.String()
	default:
		return fmt.Sprintf("%v", val)
	}
}

func (m *MQTT) publishComplex(topic string, retained bool, payload interface{}) {
	if _, ok := payload.(fmt.Stringer); ok || payload == nil {
		m.publishSingleValue(topic, retained, payload)
		return
	}

	if mm, ok := payload.(MQTTMarshaler); ok {
		if b, err := mm.MarshalMQTT(); err == nil {
			m.publishSingleValue(topic, retained, string(b))
		} else {
			m.log.ERROR.Printf("marshal mqtt: %v", err)
		}
		return
	}

	switch typ := reflect.TypeOf(payload); typ.Kind() {
	case reflect.Slice:
		// publish count
		val := reflect.ValueOf(payload)
		m.publishSingleValue(topic, retained, val.Len())

		// loop slice
		for i := range val.Len() {
			m.publishComplex(fmt.Sprintf("%s/%d", topic, i+1), retained, val.Index(i).Interface())
		}

	case reflect.Map:
		// loop map
		for iter := reflect.ValueOf(payload).MapRange(); iter.Next(); {
			k := iter.Key().String()
			m.publishComplex(fmt.Sprintf("%s/%s", topic, k), retained, iter.Value().Interface())
		}

	case reflect.Struct:
		val := reflect.ValueOf(payload)
		typ := val.Type()

		// loop struct
		for i := range typ.NumField() {
			if f := typ.Field(i); f.IsExported() {
				topic := fmt.Sprintf("%s/%s", topic, strings.ToLower(f.Name[:1])+f.Name[1:])

				if val.Field(i).IsZero() && omitEmpty(f) {
					m.publishSingleValue(topic, retained, nil)
				} else {
					m.publishComplex(topic, retained, val.Field(i).Interface())
				}
			}
		}

	case reflect.Pointer:
		if val := reflect.ValueOf(payload); !val.IsNil() {
			m.publishComplex(topic, retained, reflect.Indirect(val).Interface())
			return
		}

		payload = nil
		fallthrough

	default:
		m.publishSingleValue(topic, retained, payload)
	}
}

func (m *MQTT) publishString(topic string, retained bool, payload string) {
	m.Handler.Publish(topic, retained, m.encode(payload))
}

func (m *MQTT) publishSingleValue(topic string, retained bool, payload interface{}) {
	m.publisher(topic, retained, m.encode(payload))
}

func (m *MQTT) publish(topic string, retained bool, payload interface{}) {
	// publish phase values
	if slice, ok := payload.([]float64); ok && len(slice) == 3 {
		var total float64
		for i, v := range slice {
			total += v
			m.publishSingleValue(fmt.Sprintf("%s/l%d", topic, i+1), retained, v)
		}

		// publish sum value
		m.publishSingleValue(topic, retained, total)

		return
	}

	m.publishComplex(topic, retained, payload)
}

func (m *MQTT) Listen(site site.API) error {
	if err := m.listenSiteSetters(m.root+"/site", site); err != nil {
		return err
	}

	// loadpoint setters
	for id, lp := range site.Loadpoints() {
		topic := fmt.Sprintf("%s/loadpoints/%d", m.root, id+1)
		if err := m.listenLoadpointSetters(topic, site, lp); err != nil {
			return err
		}
	}

	// vehicle setters
	for _, vehicle := range site.Vehicles().Settings() {
		topic := fmt.Sprintf("%s/vehicles/%s", m.root, vehicle.Name())
		if err := m.listenVehicleSetters(topic, vehicle); err != nil {
			return err
		}
	}

	return nil
}

func (m *MQTT) listenSiteSetters(topic string, site site.API) error {
	for _, s := range []setter{
		{"bufferSoc", floatSetter(site.SetBufferSoc)},
		{"bufferStartSoc", floatSetter(site.SetBufferStartSoc)},
		{"batteryDischargeControl", boolSetter(site.SetBatteryDischargeControl)},
		{"prioritySoc", floatSetter(site.SetPrioritySoc)},
		{"residualPower", floatSetter(site.SetResidualPower)},
		{"smartCostLimit", floatPtrSetter(pass(func(limit *float64) {
			for _, lp := range site.Loadpoints() {
				lp.SetSmartCostLimit(limit)
			}
		}))},
		{"smartFeedInPriorityLimit", floatPtrSetter(pass(func(limit *float64) {
			for _, lp := range site.Loadpoints() {
				lp.SetSmartFeedInPriorityLimit(limit)
			}
		}))},
		{"batteryGridChargeLimit", floatPtrSetter(pass(site.SetBatteryGridChargeLimit))},
		{"batteryMode", ptrSetter(api.BatteryModeString, pass(func(m *api.BatteryMode) {
			if m == nil {
				m = lo.ToPtr(api.BatteryUnknown)
			}
			site.SetBatteryModeExternal(*m)
		}))},
	} {
		if err := m.Handler.ListenSetter(topic+"/"+s.topic, s.fun); err != nil {
			return err
		}
	}

	return nil
}

func (m *MQTT) listenLoadpointSetters(topic string, site site.API, lp loadpoint.API) error {
	for _, s := range []setter{
		{"mode", setterFunc(api.ChargeModeString, pass(lp.SetMode))},
		{"phases", intSetter(lp.SetPhasesConfigured)},
		{"limitSoc", intSetter(pass(lp.SetLimitSoc))},
		{"priority", intSetter(pass(lp.SetPriority))},
		{"minCurrent", floatSetter(lp.SetMinCurrent)},
		{"maxCurrent", floatSetter(lp.SetMaxCurrent)},
		{"limitEnergy", floatSetter(pass(lp.SetLimitEnergy))},
		{"enableThreshold", floatSetter(pass(lp.SetEnableThreshold))},
		{"disableThreshold", floatSetter(pass(lp.SetDisableThreshold))},
		{"enableDelay", durationSetter(pass(lp.SetEnableDelay))},
		{"disableDelay", durationSetter(pass(lp.SetDisableDelay))},
		{"smartCostLimit", floatPtrSetter(pass(lp.SetSmartCostLimit))},
		{"smartFeedInPriorityLimit", floatPtrSetter(pass(lp.SetSmartFeedInPriorityLimit))},
		{"batteryBoost", boolSetter(lp.SetBatteryBoost)},
		{"planEnergy", func(payload string) error {
			var plan struct {
				Time         time.Time `json:"time"`
				Precondition int64     `json:"precondition"`
				Value        float64   `json:"value"`
			}
			err := json.Unmarshal([]byte(payload), &plan)
			if err == nil {
				err = lp.SetPlanEnergy(plan.Time, time.Duration(plan.Precondition)*time.Second, plan.Value)
			}
			return err
		}},
		{"vehicle", func(payload string) error {
			// https://github.com/evcc-io/evcc/issues/11184 empty payload is swallowed by listener
			if isEmpty(payload) {
				lp.SetVehicle(nil)
				return nil
			}
			vehicle, err := site.Vehicles().ByName(payload)
			if err == nil {
				lp.SetVehicle(vehicle.Instance())
			}
			return err
		}},
	} {
		if err := m.Handler.ListenSetter(topic+"/"+s.topic, s.fun); err != nil {
			return err
		}
	}

	return nil
}

func (m *MQTT) listenVehicleSetters(topic string, v vehicle.API) error {
	for _, s := range []setter{
		{"limitSoc", intSetter(pass(v.SetLimitSoc))},
		{"minSoc", intSetter(pass(v.SetMinSoc))},
		{"planSoc", func(payload string) error {
			var plan struct {
				Time         time.Time `json:"time"`
				Precondition int64     `json:"precondition"`
				Value        int       `json:"value"`
			}
			err := json.Unmarshal([]byte(payload), &plan)
			if err == nil {
				err = v.SetPlanSoc(plan.Time, time.Duration(plan.Precondition)*time.Second, plan.Value)
			}
			return err
		}},
	} {
		if err := m.Handler.ListenSetter(topic+"/"+s.topic, s.fun); err != nil {
			return err
		}
	}

	return nil
}

// Run starts the MQTT publisher for the MQTT API
func (m *MQTT) Run(site site.API, in <-chan util.Param) {
	// number of loadpoints
	topic := fmt.Sprintf("%s/loadpoints", m.root)
	m.publish(topic, true, len(site.Loadpoints()))

	// number of vehicles
	topic = fmt.Sprintf("%s/vehicles", m.root)
	m.publish(topic, true, len(site.Vehicles().Settings()))

	for i := range 10 {
		m.publish(fmt.Sprintf("%s/site/pv/%d", m.root, i), true, nil)
		m.publish(fmt.Sprintf("%s/site/battery/%d", m.root, i), true, nil)
		m.publish(fmt.Sprintf("%s/site/vehicles/%d", m.root, i), true, nil)
	}

	// alive indicator
	var updated time.Time

	// publish
	for p := range in {
		switch {
		case p.Loadpoint != nil:
			id := *p.Loadpoint + 1
			topic = fmt.Sprintf("%s/loadpoints/%d/%s", m.root, id, p.Key)
		case p.Key == "vehicles":
			topic = fmt.Sprintf("%s/vehicles", m.root)
		default:
			topic = fmt.Sprintf("%s/site/%s", m.root, p.Key)
		}

		// alive indicator
		if time.Since(updated) > time.Second {
			updated = time.Now()
			m.publish(fmt.Sprintf("%s/updated", m.root), true, updated.Unix())
		}

		// value
		m.publish(topic, true, p.Val)
	}
}
