package proto

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
)

// PropertyID identifies an MQTT 5 property. Property values use the natural
// Go wire type: byte, uint16, uint32, VarInt, string, []byte, or StringPair.
type PropertyID byte
type VarInt uint32
type StringPair struct{ Key, Value string }
type Property struct {
	ID    PropertyID
	Value any
}
type Properties []Property

const (
	PropertyPayloadFormatIndicator     PropertyID = 0x01
	PropertyMessageExpiryInterval      PropertyID = 0x02
	PropertyContentType                PropertyID = 0x03
	PropertyResponseTopic              PropertyID = 0x08
	PropertyCorrelationData            PropertyID = 0x09
	PropertySubscriptionIdentifier     PropertyID = 0x0B
	PropertySessionExpiryInterval      PropertyID = 0x11
	PropertyAssignedClientIdentifier   PropertyID = 0x12
	PropertyServerKeepAlive            PropertyID = 0x13
	PropertyAuthenticationMethod       PropertyID = 0x15
	PropertyAuthenticationData         PropertyID = 0x16
	PropertyRequestProblemInformation  PropertyID = 0x17
	PropertyWillDelayInterval          PropertyID = 0x18
	PropertyRequestResponseInformation PropertyID = 0x19
	PropertyResponseInformation        PropertyID = 0x1A
	PropertyServerReference            PropertyID = 0x1C
	PropertyReasonString               PropertyID = 0x1F
	PropertyReceiveMaximum             PropertyID = 0x21
	PropertyTopicAliasMaximum          PropertyID = 0x22
	PropertyTopicAlias                 PropertyID = 0x23
	PropertyMaximumQoS                 PropertyID = 0x24
	PropertyRetainAvailable            PropertyID = 0x25
	PropertyUser                       PropertyID = 0x26
	// PropertyUserProperty is the specification name. PropertyUser remains as
	// a shorter backwards-compatible spelling.
	PropertyUserProperty                    PropertyID = PropertyUser
	PropertyMaximumPacketSize               PropertyID = 0x27
	PropertyWildcardSubscriptionAvailable   PropertyID = 0x28
	PropertySubscriptionIdentifierAvailable PropertyID = 0x29
	PropertySharedSubscriptionAvailable     PropertyID = 0x2A
)

type propertyWireType byte

const (
	propertyByte propertyWireType = iota
	propertyUint16
	propertyUint32
	propertyVarInt
	propertyBinary
	propertyString
	propertyStringPair
)

var propertyTypes = map[PropertyID]propertyWireType{
	0x01: propertyByte, 0x02: propertyUint32, 0x03: propertyString,
	0x08: propertyString, 0x09: propertyBinary, 0x0B: propertyVarInt,
	0x11: propertyUint32, 0x12: propertyString, 0x13: propertyUint16,
	0x15: propertyString, 0x16: propertyBinary, 0x17: propertyByte,
	0x18: propertyUint32, 0x19: propertyByte, 0x1A: propertyString,
	0x1C: propertyString, 0x1F: propertyString, 0x21: propertyUint16,
	0x22: propertyUint16, 0x23: propertyUint16, 0x24: propertyByte,
	0x25: propertyByte, 0x26: propertyStringPair, 0x27: propertyUint32,
	0x28: propertyByte, 0x29: propertyByte, 0x2A: propertyByte,
}

type propertyContext uint8

const (
	propertiesConnect propertyContext = iota
	propertiesWill
	propertiesConnAck
	propertiesPublish
	propertiesAck
	propertiesSubscribe
	propertiesSubAck
	propertiesUnsubscribe
	propertiesUnsubAck
	propertiesDisconnect
	propertiesAuth
)

func propertySet(ids ...PropertyID) map[PropertyID]struct{} {
	out := make(map[PropertyID]struct{}, len(ids))
	for _, id := range ids {
		out[id] = struct{}{}
	}
	return out
}

var allowedProperties = map[propertyContext]map[PropertyID]struct{}{
	propertiesConnect: propertySet(
		PropertySessionExpiryInterval, PropertyReceiveMaximum, PropertyMaximumPacketSize,
		PropertyTopicAliasMaximum, PropertyRequestResponseInformation, PropertyRequestProblemInformation,
		PropertyUserProperty, PropertyAuthenticationMethod, PropertyAuthenticationData,
	),
	propertiesWill: propertySet(
		PropertyWillDelayInterval, PropertyPayloadFormatIndicator, PropertyMessageExpiryInterval,
		PropertyContentType, PropertyResponseTopic, PropertyCorrelationData, PropertyUserProperty,
	),
	propertiesConnAck: propertySet(
		PropertySessionExpiryInterval, PropertyReceiveMaximum, PropertyMaximumQoS, PropertyRetainAvailable,
		PropertyMaximumPacketSize, PropertyAssignedClientIdentifier, PropertyTopicAliasMaximum,
		PropertyReasonString, PropertyUserProperty, PropertyWildcardSubscriptionAvailable,
		PropertySubscriptionIdentifierAvailable, PropertySharedSubscriptionAvailable, PropertyServerKeepAlive,
		PropertyResponseInformation, PropertyServerReference, PropertyAuthenticationMethod, PropertyAuthenticationData,
	),
	propertiesPublish: propertySet(
		PropertyPayloadFormatIndicator, PropertyMessageExpiryInterval, PropertyTopicAlias,
		PropertyResponseTopic, PropertyCorrelationData, PropertyUserProperty,
		PropertySubscriptionIdentifier, PropertyContentType,
	),
	propertiesAck:         propertySet(PropertyReasonString, PropertyUserProperty),
	propertiesSubscribe:   propertySet(PropertySubscriptionIdentifier, PropertyUserProperty),
	propertiesSubAck:      propertySet(PropertyReasonString, PropertyUserProperty),
	propertiesUnsubscribe: propertySet(PropertyUserProperty),
	propertiesUnsubAck:    propertySet(PropertyReasonString, PropertyUserProperty),
	propertiesDisconnect: propertySet(PropertySessionExpiryInterval, PropertyReasonString, PropertyUserProperty, PropertyServerReference),
	propertiesAuth:        propertySet(PropertyAuthenticationMethod, PropertyAuthenticationData, PropertyReasonString, PropertyUserProperty),
}

func validateProperties(ctx propertyContext, props Properties) error {
	allowed := allowedProperties[ctx]
	seen := make(map[PropertyID]int, len(props))
	hasAuthMethod := false
	hasAuthData := false
	for _, p := range props {
		if _, ok := allowed[p.ID]; !ok {
			return fmt.Errorf("mqtt: property 0x%x is not valid in this packet", byte(p.ID))
		}
		seen[p.ID]++
		repeatable := p.ID == PropertyUserProperty || (ctx == propertiesPublish && p.ID == PropertySubscriptionIdentifier)
		if seen[p.ID] > 1 && !repeatable {
			return fmt.Errorf("mqtt: property 0x%x must not appear more than once", byte(p.ID))
		}

		switch p.ID {
		case PropertyPayloadFormatIndicator, PropertyRequestProblemInformation, PropertyRequestResponseInformation,
			PropertyMaximumQoS, PropertyRetainAvailable, PropertyWildcardSubscriptionAvailable,
			PropertySubscriptionIdentifierAvailable, PropertySharedSubscriptionAvailable:
			v, ok := p.Value.(byte)
			if !ok {
				return propertyTypeError(p, "byte")
			}
			if v > 1 {
				return fmt.Errorf("mqtt: property 0x%x must be 0 or 1", byte(p.ID))
			}
		case PropertyReceiveMaximum:
			v, ok := p.Value.(uint16)
			if !ok {
				return propertyTypeError(p, "uint16")
			}
			if v == 0 {
				return errors.New("mqtt: receive maximum must be non-zero")
			}
		case PropertyTopicAlias:
			v, ok := p.Value.(uint16)
			if !ok {
				return propertyTypeError(p, "uint16")
			}
			if v == 0 {
				return errors.New("mqtt: topic alias must be non-zero")
			}
		case PropertyMaximumPacketSize:
			v, ok := p.Value.(uint32)
			if !ok {
				return propertyTypeError(p, "uint32")
			}
			if v == 0 {
				return errors.New("mqtt: maximum packet size must be non-zero")
			}
		case PropertySubscriptionIdentifier:
			v, ok := p.Value.(VarInt)
			if !ok {
				return propertyTypeError(p, "proto.VarInt")
			}
			if v == 0 || uint32(v) > uint32(MaxPayloadSize) {
				return errors.New("mqtt: subscription identifier is out of range")
			}
		case PropertyResponseTopic:
			v, ok := p.Value.(string)
			if !ok {
				return propertyTypeError(p, "string")
			}
			if v == "" || strings.ContainsAny(v, "+#") {
				return errors.New("mqtt: response topic must be a non-empty topic name")
			}
		case PropertyAssignedClientIdentifier:
			v, ok := p.Value.(string)
			if !ok {
				return propertyTypeError(p, "string")
			}
			if v == "" {
				return errors.New("mqtt: assigned client identifier must be non-empty")
			}
		case PropertyAuthenticationMethod:
			hasAuthMethod = true
		case PropertyAuthenticationData:
			hasAuthData = true
		}
	}
	if hasAuthData && !hasAuthMethod {
		return errors.New("mqtt: authentication data requires authentication method")
	}
	return nil
}

func (p Properties) Add(id PropertyID, value any) Properties {
	return append(p, Property{ID: id, Value: value})
}
func (p Properties) Values(id PropertyID) []any {
	var out []any
	for _, v := range p {
		if v.ID == id {
			out = append(out, v.Value)
		}
	}
	return out
}

func encodeProperties(dst *bytes.Buffer, props Properties, ctx propertyContext) error {
	if err := validateProperties(ctx, props); err != nil {
		return err
	}
	var body bytes.Buffer
	for _, p := range props {
		wt, ok := propertyTypes[p.ID]
		if !ok {
			return fmt.Errorf("mqtt: unknown property 0x%x", byte(p.ID))
		}
		encodeLength(int32(p.ID), &body)
		switch wt {
		case propertyByte:
			v, ok := p.Value.(byte)
			if !ok {
				return propertyTypeError(p, "byte")
			}
			body.WriteByte(v)
		case propertyUint16:
			v, ok := p.Value.(uint16)
			if !ok {
				return propertyTypeError(p, "uint16")
			}
			setUint16(v, &body)
		case propertyUint32:
			v, ok := p.Value.(uint32)
			if !ok {
				return propertyTypeError(p, "uint32")
			}
			setUint32(v, &body)
		case propertyVarInt:
			v, ok := p.Value.(VarInt)
			if !ok {
				return propertyTypeError(p, "proto.VarInt")
			}
			encodeLength(int32(v), &body)
		case propertyBinary:
			v, ok := p.Value.([]byte)
			if !ok {
				return propertyTypeError(p, "[]byte")
			}
			setBinary(v, &body)
		case propertyString:
			v, ok := p.Value.(string)
			if !ok {
				return propertyTypeError(p, "string")
			}
			setString(v, &body)
		case propertyStringPair:
			v, ok := p.Value.(StringPair)
			if !ok {
				return propertyTypeError(p, "proto.StringPair")
			}
			setString(v.Key, &body)
			setString(v.Value, &body)
		}
	}
	encodeLength(int32(body.Len()), dst)
	dst.Write(body.Bytes())
	return nil
}

func propertyTypeError(p Property, want string) error {
	return fmt.Errorf("mqtt: property 0x%x requires %s, got %T", byte(p.ID), want, p.Value)
}

func decodeProperties(r io.Reader, remaining *int32, ctx propertyContext) Properties {
	n := decodeLengthCounted(r, remaining)
	if n > *remaining {
		raiseError(dataExceedsPacketError)
	}
	pr := &io.LimitedReader{R: r, N: int64(n)}
	pn := int32(n)
	var props Properties
	for pn > 0 {
		id := PropertyID(decodeLengthCounted(pr, &pn))
		wt, ok := propertyTypes[id]
		if !ok {
			raiseError(fmt.Errorf("mqtt: unknown property 0x%x", byte(id)))
		}
		var value any
		switch wt {
		case propertyByte:
			value = getUint8(pr, &pn)
		case propertyUint16:
			value = getUint16(pr, &pn)
		case propertyUint32:
			value = getUint32(pr, &pn)
		case propertyVarInt:
			value = VarInt(decodeLengthCounted(pr, &pn))
		case propertyBinary:
			value = getBinary(pr, &pn)
		case propertyString:
			value = getString(pr, &pn)
		case propertyStringPair:
			value = StringPair{getString(pr, &pn), getString(pr, &pn)}
		}
		props = append(props, Property{ID: id, Value: value})
	}
	*remaining -= int32(n)
	if err := validateProperties(ctx, props); err != nil {
		raiseError(err)
	}
	return props
}
