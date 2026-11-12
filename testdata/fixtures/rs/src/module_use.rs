use crate::util;

pub fn measure(value: u32) -> u32 {
    util::clamp(value, 0, 2)
}
