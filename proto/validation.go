package proto

import (
	"errors"
	"strings"
)

func validateTopicName(topic string, allowEmpty bool) error {
	if err := validateUTF8String(topic); err != nil {
		return err
	}
	if topic == "" && !allowEmpty {
		return errors.New("mqtt: topic name must not be empty")
	}
	if strings.ContainsAny(topic, "+#") {
		return errors.New("mqtt: topic name must not contain wildcards")
	}
	return nil
}

func validateTopicFilter(filter string) error {
	if err := validateUTF8String(filter); err != nil {
		return err
	}
	if filter == "" {
		return errors.New("mqtt: topic filter must not be empty")
	}
	if strings.HasPrefix(filter, "$share/") {
		rest := strings.TrimPrefix(filter, "$share/")
		i := strings.IndexByte(rest, '/')
		if i <= 0 || i == len(rest)-1 {
			return errors.New("mqtt: invalid shared subscription")
		}
		group, inner := rest[:i], rest[i+1:]
		if strings.ContainsAny(group, "+#") || strings.HasPrefix(inner, "$share/") {
			return errors.New("mqtt: invalid shared subscription")
		}
		return validateTopicFilter(inner)
	}
	levels := strings.Split(filter, "/")
	for i, level := range levels {
		if strings.Contains(level, "#") && (level != "#" || i != len(levels)-1) {
			return errors.New("mqtt: multi-level wildcard must occupy the final level")
		}
		if strings.Contains(level, "+") && level != "+" {
			return errors.New("mqtt: single-level wildcard must occupy an entire level")
		}
	}
	return nil
}

func hasProperty(props Properties, id PropertyID) bool {
	for _, p := range props {
		if p.ID == id {
			return true
		}
	}
	return false
}
