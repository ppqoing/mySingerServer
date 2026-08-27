//! 不排序输入的 Torben 中位数，逐分支对应 Meta PDQ 上游实现。

/// 返回非空切片的 Torben 中位数。
pub(super) fn torben(values: &[f32]) -> f32 {
    let mut minimum = values[0];
    let mut maximum = values[0];
    for value in &values[1..] {
        if *value < minimum {
            minimum = *value;
        }
        if *value > maximum {
            maximum = *value;
        }
    }

    loop {
        let guess = (minimum + maximum) / 2.0;
        let mut less = 0_usize;
        let mut greater = 0_usize;
        let mut equal = 0_usize;
        let mut greatest_below_guess = minimum;
        let mut smallest_above_guess = maximum;

        for value in values {
            if *value < guess {
                less += 1;
                if *value > greatest_below_guess {
                    greatest_below_guess = *value;
                }
            } else if *value > guess {
                greater += 1;
                if *value < smallest_above_guess {
                    smallest_above_guess = *value;
                }
            } else {
                equal += 1;
            }
        }

        let middle = values.len().div_ceil(2);
        if less <= middle && greater <= middle {
            return if less >= middle {
                greatest_below_guess
            } else if less + equal >= middle {
                guess
            } else {
                smallest_above_guess
            };
        }
        if less > greater {
            maximum = greatest_below_guess;
        } else {
            minimum = smallest_above_guess;
        }
    }
}
