pub fn clamp(value: u32, low: u32, high: u32) -> u32 {
    if value < low {
        low
    } else if value > high {
        high
    } else {
        value
    }
}

pub fn helper(x: u32) -> u32 {
    clamp(x, 0, 10)
}
