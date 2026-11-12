pub mod models;
pub mod store;
pub mod util;
pub mod service;
pub mod assoc;
pub mod glob_use;
pub mod alias_use;
pub mod module_use;
pub mod reexport_use;
pub mod entry;
pub mod global_use;
pub mod widget;
pub mod amb_a;
pub mod amb_b;
pub mod amb_use;
pub mod macros;

pub use crate::models::User;

pub mod inline {
    pub fn inner() -> u32 {
        1
    }
}

pub fn root_helper() -> u32 {
    crate::util::clamp(1, 0, 2)
}
