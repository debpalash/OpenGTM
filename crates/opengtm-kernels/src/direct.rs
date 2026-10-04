//! A direct memory ABI for the in-process Go host.
//!
//! The Extism exports in `plugin` move input and output through the Extism
//! runtime with one host call per 8 bytes, which dominates the cost of small
//! kernels (see BENCHMARKS.md). The Go server instead writes the request into
//! guest memory and reads the response back in one copy each:
//!
//! 1. `ptr = og_alloc(len)`; the host writes `len` request bytes at `ptr`.
//! 2. `packed = og_<function>(ptr, len)`; the guest takes ownership of the
//!    request buffer and frees it. `packed` is `out_ptr << 32 | out_len`.
//! 3. The host reads `out_len` bytes at `out_ptr`, then `og_free(out_ptr, out_len)`.
//!
//! Requests and responses are the same JSON as the Extism exports.

use crate::api;

/// Allocates `len` bytes for a request. The buffer must be passed to exactly
/// one `og_*` call (which frees it) or released with [`og_free`].
#[unsafe(no_mangle)]
pub extern "C" fn og_alloc(len: u32) -> u32 {
    let mut buf = Vec::<u8>::with_capacity(len as usize);
    let ptr = buf.as_mut_ptr();
    std::mem::forget(buf);
    ptr as u32
}

/// Frees a buffer returned by [`og_alloc`] or by an `og_*` call.
///
/// # Safety
/// `ptr`/`len` must describe a live buffer from this module, freed once.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn og_free(ptr: u32, len: u32) {
    // SAFETY: every buffer handed out has capacity == len (see og_alloc and
    // run, which returns a boxed slice).
    drop(unsafe { Vec::from_raw_parts(ptr as *mut u8, 0, len as usize) });
}

fn run(f: fn(&[u8]) -> Vec<u8>, ptr: u32, len: u32) -> u64 {
    // SAFETY: the host wrote `len` bytes into a buffer from og_alloc(len) and
    // transfers ownership to this call.
    let input = unsafe { Vec::from_raw_parts(ptr as *mut u8, len as usize, len as usize) };
    let out = f(&input).into_boxed_slice();
    drop(input);
    let out_len = out.len() as u64;
    let out_ptr = Box::into_raw(out) as *mut u8 as u32 as u64;
    (out_ptr << 32) | out_len
}

#[unsafe(no_mangle)]
pub extern "C" fn og_normalize_domain(ptr: u32, len: u32) -> u64 {
    run(api::normalize_domain, ptr, len)
}

#[unsafe(no_mangle)]
pub extern "C" fn og_normalize_email(ptr: u32, len: u32) -> u64 {
    run(api::normalize_email, ptr, len)
}

#[unsafe(no_mangle)]
pub extern "C" fn og_normalize_phone(ptr: u32, len: u32) -> u64 {
    run(api::normalize_phone, ptr, len)
}

#[unsafe(no_mangle)]
pub extern "C" fn og_normalize_person_name(ptr: u32, len: u32) -> u64 {
    run(api::normalize_person_name, ptr, len)
}

#[unsafe(no_mangle)]
pub extern "C" fn og_normalize_batch(ptr: u32, len: u32) -> u64 {
    run(api::normalize_batch, ptr, len)
}

#[unsafe(no_mangle)]
pub extern "C" fn og_extract(ptr: u32, len: u32) -> u64 {
    run(api::extract, ptr, len)
}
