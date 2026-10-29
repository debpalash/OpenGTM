package api

// Static SCIM discovery documents, copied from legacy routers/scim.py.

const (
	scimUserSchema         = "urn:ietf:params:scim:schemas:core:2.0:User"
	scimGroupSchema        = "urn:ietf:params:scim:schemas:core:2.0:Group"
	scimListSchema         = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	scimPatchSchema        = "urn:ietf:params:scim:api:messages:2.0:PatchOp"
	scimSchemaSchema       = "urn:ietf:params:scim:schemas:core:2.0:Schema"
	scimResourceTypeSchema = "urn:ietf:params:scim:schemas:core:2.0:ResourceType"
)

type jsonObj = map[string]any

func scimServiceProviderConfig() jsonObj {
	return jsonObj{
		"schemas":        []string{"urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"},
		"patch":          jsonObj{"supported": true},
		"bulk":           jsonObj{"supported": false},
		"filter":         jsonObj{"supported": true, "maxResults": 200},
		"changePassword": jsonObj{"supported": false},
		"sort":           jsonObj{"supported": false},
		"etag":           jsonObj{"supported": false},
		"authenticationSchemes": []jsonObj{{
			"type": "oauthbearertoken", "name": "Bearer Token", "description": "Workspace-scoped SCIM token",
			"specUri": "https://www.rfc-editor.org/info/rfc6750", "primary": true,
		}},
	}
}

func scimResourceType(slug, kind, schema string) jsonObj {
	return jsonObj{
		"schemas": []string{scimResourceTypeSchema}, "id": kind, "name": kind, "endpoint": "/" + kind + "s",
		"schema": schema, "schemaExtensions": []any{},
		"meta": jsonObj{"resourceType": "ResourceType", "location": "/scim/v2/" + slug + "/ResourceTypes/" + kind},
	}
}

// scimResourceTypeSchemas maps resource type name to its schema urn, in listing order.
var scimResourceTypeOrder = []struct{ kind, schema string }{{"User", scimUserSchema}, {"Group", scimGroupSchema}}

func scimAttr(name, typ string, over jsonObj) jsonObj {
	a := jsonObj{
		"multiValued": false, "required": false, "caseExact": false, "mutability": "readWrite",
		"returned": "default", "uniqueness": "none", "name": name, "type": typ,
	}
	for k, v := range over {
		a[k] = v
	}
	return a
}

func scimSchemaResources() []jsonObj {
	return []jsonObj{
		{"schemas": []string{scimSchemaSchema}, "id": scimUserSchema, "name": "User", "description": "OpenGTM workspace user", "attributes": []jsonObj{
			scimAttr("userName", "string", jsonObj{"required": true, "uniqueness": "server"}),
			scimAttr("displayName", "string", nil), scimAttr("externalId", "string", nil), scimAttr("active", "boolean", nil),
		}},
		{"schemas": []string{scimSchemaSchema}, "id": scimGroupSchema, "name": "Group", "description": "OpenGTM workspace group", "attributes": []jsonObj{
			scimAttr("displayName", "string", jsonObj{"required": true, "uniqueness": "server"}),
			scimAttr("externalId", "string", nil),
			scimAttr("members", "complex", jsonObj{"multiValued": true, "subAttributes": []jsonObj{
				scimAttr("value", "string", jsonObj{"mutability": "immutable"}),
				scimAttr("$ref", "reference", jsonObj{"referenceTypes": []string{"User"}, "mutability": "immutable"}),
			}}),
		}},
	}
}
