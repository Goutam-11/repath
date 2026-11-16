use extra_lib::extra_helper;

pub fn measure(value: u32) -> u32 {
    extra_helper(value)
}

pub fn direct(value: u32) -> u32 {
    extra_lib::model::clamp2(value)
}
