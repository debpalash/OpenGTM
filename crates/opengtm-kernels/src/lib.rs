//! OpenGTM data-plane kernels.
//!
//! The pure logic lives in [`normalize`], [`extract`] and [`api`] and is tested
//! natively. The Extism exports in `plugin` and the direct-memory exports in
//! `direct` (both wasm32 only) are thin wrappers around [`api`], which takes
//! and returns JSON bytes.

pub mod api;
pub mod extract;
pub mod normalize;
mod pystr;
mod pyurl;

#[cfg(target_arch = "wasm32")]
mod direct;
#[cfg(target_arch = "wasm32")]
mod plugin;
