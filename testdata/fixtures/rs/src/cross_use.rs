use crate::models::Cross;

pub fn run(value: &Cross) -> u32 {
    value.cross()
}

pub fn build() -> Cross {
    Cross::make()
}
