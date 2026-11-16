#[path = "alt/impl_file.rs"]
mod alt_mod;

use self::alt_mod::alt_helper;

pub fn via_attr(value: u32) -> u32 {
    crate::path_use::alt_mod::alt_helper(value)
}

pub fn via_use(value: u32) -> u32 {
    alt_helper(value)
}
