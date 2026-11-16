use crate::inline::inner;

pub fn call_inline() -> u32 {
    inner()
}

pub fn direct_inline() -> u32 {
    crate::inline::inner()
}
