//! Extism exports. Each one is a thin wrapper over [`crate::api`]; errors are
//! reported in the JSON body (see `api`), so every call returns success unless
//! the module traps.

use extism_pdk::{FnResult, plugin_fn};

use crate::api;

#[plugin_fn]
pub fn normalize_domain(input: Vec<u8>) -> FnResult<Vec<u8>> {
    Ok(api::normalize_domain(&input))
}

#[plugin_fn]
pub fn normalize_email(input: Vec<u8>) -> FnResult<Vec<u8>> {
    Ok(api::normalize_email(&input))
}

#[plugin_fn]
pub fn normalize_phone(input: Vec<u8>) -> FnResult<Vec<u8>> {
    Ok(api::normalize_phone(&input))
}

#[plugin_fn]
pub fn normalize_person_name(input: Vec<u8>) -> FnResult<Vec<u8>> {
    Ok(api::normalize_person_name(&input))
}

#[plugin_fn]
pub fn normalize_batch(input: Vec<u8>) -> FnResult<Vec<u8>> {
    Ok(api::normalize_batch(&input))
}

#[plugin_fn]
pub fn extract(input: Vec<u8>) -> FnResult<Vec<u8>> {
    Ok(api::extract(&input))
}
