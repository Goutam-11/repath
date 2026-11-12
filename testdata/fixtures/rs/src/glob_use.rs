use crate::util::*;

pub fn bounded(value: u32) -> u32 {
    clamp(value, 0, 1)
}
