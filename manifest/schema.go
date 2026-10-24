package manifest

// The JSON Schema of an Environment manifest, for editors and CI.
//
// An editor that knows it completes keys, flags a misspelled one and shows the
// help for each, before the file ever reaches Sybil — VS Code reads it from a
// `# yaml-language-server: $schema=<url>` line. It describes the shape only:
// whether the manifest makes sense against this org (a connector that exists,
// a blueprint's inputs, a free address) is the plan's to say, and the plan is
// still the authority.
//
// Written here, beside the wire structs it describes, and pinned to them by a
// test that every yaml key of every wire struct is a property here.

// EnvironmentJSONSchema returns the schema as a JSON-encodable value.
func EnvironmentJSONSchema() map[string]any {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	name := map[string]any{"type": "string", "pattern": `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`}

	bootstrap := map[string]any{
		"type":                 "object",
		"description":          "First-boot setup, after Sybil's own enrollment. No password field on purpose; reference secrets as {{secret.name}}.",
		"additionalProperties": false,
		"properties": map[string]any{
			"users": map[string]any{"type": "array", "items": map[string]any{
				"type": "object", "additionalProperties": false, "required": []string{"name"},
				"properties": map[string]any{
					"name":     map[string]any{"type": "string", "pattern": `^[a-z_][a-z0-9_-]{0,31}$`},
					"ssh_keys": map[string]any{"type": "array", "items": str("A public key: ssh-ed25519 AAAA… comment")},
					"sudo":     map[string]any{"type": "boolean", "description": "Passwordless sudo."},
					"shell":    str("An absolute path, e.g. /bin/bash."),
				},
			}},
			"packages": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"timezone": str("e.g. Europe/Paris"),
			"files": map[string]any{"type": "array", "items": map[string]any{
				"type": "object", "additionalProperties": false, "required": []string{"path", "content"},
				"properties": map[string]any{
					"path":        str("A clean absolute path."),
					"content":     map[string]any{"type": "string"},
					"permissions": map[string]any{"type": "string", "pattern": `^0?[0-7]{3}$`},
					"owner":       str("user or user:group"),
				},
			}},
			"run": map[string]any{"type": "array", "items": str("A shell command, run once after Sybil's agent is installed.")},
		},
	}

	network := map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"name", "subnet"},
		"properties": map[string]any{
			"name":         map[string]any{"type": "string", "pattern": `^[a-z][a-z0-9]{0,7}$`, "description": "1-8 lowercase letters and digits: it becomes an interface name."},
			"zone":         map[string]any{"type": "string", "pattern": `^[a-z][a-z0-9]{0,7}$`, "default": DefaultNetworkZone},
			"subnet":       str("A canonical CIDR, e.g. 10.20.0.0/24."),
			"gateway":      str("Defaults to the subnet's first address."),
			"snat":         map[string]any{"type": "boolean", "default": true, "description": "Outbound access through the node."},
			"dhcp":         map[string]any{"type": "boolean", "default": false, "description": "Needs dnsmasq on every node."},
			"dhcp_range":   str("START-END; defaults to the upper half of the subnet."),
			"static_range": str("START-END the addresses of `ip: static` come from."),
			"dns":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Name servers for machines with a fixed address."},
		},
	}

	policy := func(def string) map[string]any {
		return map[string]any{"type": "string", "enum": []string{"ACCEPT", "DROP", "REJECT"}, "default": def}
	}
	firewall := map[string]any{
		"type":                 "object",
		"description":          "The hypervisor's firewall for each machine, below the guest. The rules are the whole list: one removed here is removed from the machine. Leave the block out to leave the firewall alone.",
		"additionalProperties": false,
		"properties": map[string]any{
			"enabled":    map[string]any{"type": "boolean", "default": true, "description": "The guest's firewall switch. The cluster's must be on too, or nothing is filtered."},
			"policy_in":  policy(DefaultFirewallPolicyIn),
			"policy_out": policy(DefaultFirewallPolicyOut),
			"rules": map[string]any{"type": "array", "maxItems": MaxFirewallRules, "items": map[string]any{
				"type": "object", "additionalProperties": false,
				"description": "Evaluated in order, first match wins. Either group:, or a rule with an action.",
				"properties": map[string]any{
					"direction": map[string]any{"type": "string", "enum": []string{"in", "out"}, "default": "in"},
					"action":    map[string]any{"type": "string", "enum": []string{"ACCEPT", "DROP", "REJECT"}},
					"group":     str("A security group defined on the cluster, applied in this rule's place."),
					"macro":     str("A named service such as SSH, HTTP or HTTPS, in place of proto and ports."),
					"proto":     str("tcp, udp, icmp…"),
					"source":    str("An address, CIDR, range, +ipset or alias; a comma-separated list of them."),
					"dest":      str("Same forms as source."),
					"dport":     str("A port, a range such as 8000:8080, or a list. Needs proto tcp or udp."),
					"sport":     str("Same forms as dport."),
					"comment":   str("One line."),
				},
			}},
		},
	}
	placement := map[string]any{
		"type": "object", "additionalProperties": false,
		"description": "Where the machines may run. With it, Sybil chooses the node (the one with the most free memory) " +
			"and the blueprint's node input is left out of inputs.",
		"properties": map[string]any{
			"node":       map[string]any{"type": "string", "default": PlacementAuto, "description": "auto, or one node's name."},
			"nodes":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "The only nodes auto may choose."},
			"spread":     map[string]any{"type": "boolean", "default": false, "description": "Every instance on its own node."},
			"apart_from": map[string]any{"type": "array", "items": name, "description": "Resources whose machines never share a node with these."},
		},
	}

	resource := map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"name", "blueprint"},
		"properties": map[string]any{
			"name":      name,
			"count":     map[string]any{"type": "integer", "minimum": 0, "maximum": MaxResourceCount, "default": 1},
			"blueprint": str("The id of the connector's blueprint that builds it, as its provider lists it."),
			"network":   str("One of spec.networks, or a bridge that already exists."),
			"ip":        map[string]any{"type": "string", "enum": []string{"dhcp", "static"}, "default": "dhcp"},
			"nics": map[string]any{
				"type": "array", "minItems": 1, "maxItems": MaxNICs,
				"description": "Several network cards, first one first; instead of network and ip. Only the first carries the default route and name servers.",
				"items": map[string]any{
					"type": "object", "additionalProperties": false, "required": []string{"network"},
					"properties": map[string]any{
						"network": str("One of spec.networks, or a bridge that already exists."),
						"ip":      map[string]any{"type": "string", "enum": []string{"dhcp", "static"}, "default": "dhcp"},
					},
				},
			},
			"power":     map[string]any{"type": "string", "enum": []string{"on", "off"}, "default": "on", "description": "Whether the machines should be running."},
			"image":     map[string]any{"type": "string", "pattern": ImagePattern.String(), "description": "A template by name, optionally @sha256:<digest>."},
			"placement": placement,
			"firewall":  firewall,
			"inputs": map[string]any{
				"type":        "object",
				"description": "The blueprint's inputs. Strings may use {{instance.number}} and {{instance.index}}.",
				"properties":  map[string]any{"bootstrap": bootstrap},
			},
		},
	}

	return map[string]any{
		"$schema":              "https://json-schema.org/draft/2020-12/schema",
		"$id":                  "https://sybil.dev/schemas/environment.sybil-v1alpha1.json",
		"title":                "Sybil Environment (sybil/v1alpha1)",
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"apiVersion", "kind", "metadata", "spec"},
		"properties": map[string]any{
			"apiVersion": map[string]any{"const": EnvironmentV1Alpha1},
			"kind":       map[string]any{"const": KindEnvironment},
			"metadata": map[string]any{
				"type": "object", "required": []string{"name"},
				"properties": map[string]any{"name": name},
			},
			"spec": map[string]any{
				"type": "object", "additionalProperties": false, "required": []string{"integration"},
				"properties": map[string]any{
					"integration": str("The connector's name in Sybil."),
					"group":       str("An asset group the machines join once accepted."),
					"images": map[string]any{
						"type": "string", "enum": []string{ImagesApprovedOnly, ImagesKnownOrWarn}, "default": ImagesApprovedOnly,
						"description": "Sybil builds only from approved images. approved-only also refuses an image whose content Sybil cannot know " +
							"(a template it did not build); known-or-warn plans that one with a warning. A known image that is not approved is refused either way.",
					},
					"variables": map[string]any{
						"type":                 "object",
						"description":          "Values used as {{var.name}} in inputs and images. A string that is only a reference takes the value's type.",
						"propertyNames":        map[string]any{"pattern": `^[A-Za-z_][A-Za-z0-9_]*$`},
						"additionalProperties": map[string]any{"type": []string{"string", "number", "boolean"}},
					},
					"networks":  map[string]any{"type": "array", "items": network},
					"resources": map[string]any{"type": "array", "items": resource},
				},
			},
		},
	}
}

// StandaloneJSONSchema is the schema of an accretion/v1alpha1 manifest:
// EnvironmentJSONSchema with spec.provider in spec.integration's place.
// Built from it, so the two shapes describe the same resources.
func StandaloneJSONSchema() map[string]any {
	s := EnvironmentJSONSchema()
	s["$id"] = "https://sybil.dev/schemas/environment.accretion-v1alpha1.json"
	s["title"] = "Accretion Environment (accretion/v1alpha1)"
	props := s["properties"].(map[string]any)
	props["apiVersion"] = map[string]any{"const": EnvironmentStandaloneV1Alpha1}
	spec := props["spec"].(map[string]any)
	spec["required"] = []string{"provider"}
	specProps := spec["properties"].(map[string]any)
	delete(specProps, "integration")
	delete(specProps, "images")
	specProps["provider"] = map[string]any{
		"type":                 "object",
		"description":          "What builds the machines. The token is never written here: name the file or the environment variable that holds it.",
		"additionalProperties": false,
		"required":             []string{"type", "endpoint"},
		"oneOf": []any{
			map[string]any{"required": []string{"token_file"}},
			map[string]any{"required": []string{"token_env"}},
		},
		"properties": map[string]any{
			"type":       map[string]any{"type": "string", "pattern": providerTypePattern.String(), "description": "The provider's registered name."},
			"endpoint":   map[string]any{"type": "string", "pattern": "^https://", "description": "The provider's API, e.g. https://host:8006."},
			"verify_tls": map[string]any{"type": "boolean", "default": true, "description": "Check the endpoint's certificate."},
			"token_file": map[string]any{"type": "string", "description": "A file holding the token, relative to this manifest. Keep it mode 0600."},
			"token_env":  map[string]any{"type": "string", "pattern": envVarPattern.String(), "description": "An environment variable holding the token."},
		},
	}
	return s
}
