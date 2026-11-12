use crate::widget::Widget;

pub trait RunB {
    fn run(&self) -> u32;
}

impl RunB for Widget {
    fn run(&self) -> u32 {
        2
    }
}
