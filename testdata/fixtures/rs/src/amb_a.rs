use crate::widget::Widget;

pub trait RunA {
    fn run(&self) -> u32;
}

impl RunA for Widget {
    fn run(&self) -> u32 {
        1
    }
}
